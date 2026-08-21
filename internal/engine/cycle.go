package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/zxcll/vps-panel/internal/billing"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

// rollCycleIfDue 检查节点的账单周期是否该翻页。
//
// 三种情况：
//   - 边界还没初始化（新建节点）：只写边界，不归档
//   - 用户改了重置日或时区：重算边界，不归档（改配置不该凭空清掉用量）
//   - 到点了：归档 → 清零 → 写新边界 → 按需恢复被关停的节点 → 切回主节点
func (e *Engine) rollCycleIfDue(ctx context.Context, n *store.Node) error {
	now := time.Now().UTC()

	// 边界为零值说明还没初始化过
	if n.CycleStart.IsZero() || n.CycleEnd.IsZero() {
		loc, _ := billing.LoadLocation(n.ResetTZ)
		start, end := billing.CurrentCycle(now, n.ResetDay, loc)
		if err := e.st.SetNodeCycle(ctx, n.ID, start, end, false); err != nil {
			return err
		}
		if err := e.st.ResetUsage(ctx, n.ID, start); err != nil {
			return err
		}
		n.CycleStart, n.CycleEnd = start, end
		return nil
	}

	if now.Before(n.CycleEnd) {
		// 还没到点。顺带检查用户是不是改了重置日/时区，改了就把边界修正过来。
		start, end, changed := billing.EnsureCycle(now, n.CycleStart, n.CycleEnd, n.ResetDay, n.ResetTZ)
		if changed {
			e.log.Info("账单周期边界已按新的重置日重算",
				"node", n.Name, "start", start, "end", end)
			if err := e.st.SetNodeCycle(ctx, n.ID, start, end, false); err != nil {
				return err
			}
			n.CycleStart, n.CycleEnd = start, end
		}
		return nil
	}

	return e.RollCycle(ctx, n, "账单周期到期自动清零")
}

// RollCycle 归档当前周期并开启新周期。手动"立即清零"也走这里。
func (e *Engine) RollCycle(ctx context.Context, n *store.Node, reason string) error {
	now := time.Now().UTC()

	usage, err := e.st.GetUsage(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("读取用量: %w", err)
	}

	billed := quota.Billed(usage.RxBytes, usage.TxBytes, n.BillingMode, n.TrafficRatio)

	cycleEnd := n.CycleEnd
	if cycleEnd.IsZero() || cycleEnd.After(now) {
		// 手动清零时周期还没到期，按"截止到此刻"归档
		cycleEnd = now
	}
	cycleStart := n.CycleStart
	if cycleStart.IsZero() {
		cycleStart = usage.CycleStart
	}

	if err := e.st.ArchiveCycle(ctx, &store.CycleRecord{
		NodeID:      n.ID,
		CycleStart:  cycleStart,
		CycleEnd:    cycleEnd,
		RxBytes:     usage.RxBytes,
		TxBytes:     usage.TxBytes,
		BilledBytes: billed,
		QuotaBytes:  n.QuotaBytes,
		BillingMode: n.BillingMode,
	}); err != nil {
		return fmt.Errorf("归档历史周期: %w", err)
	}

	// 直接按"现在"重算周期。面板停机跨过好几个周期时，
	// 这样能一步跳到当前周期，不必逐月空转。
	loc, _ := billing.LoadLocation(n.ResetTZ)
	start, end := billing.CurrentCycle(now, n.ResetDay, loc)

	if err := e.st.ResetUsage(ctx, n.ID, start); err != nil {
		return fmt.Errorf("清零用量: %w", err)
	}
	// clearHandled=true：清掉超额/预警去重标记，新周期重新开始判定
	if err := e.st.SetNodeCycle(ctx, n.ID, start, end, true); err != nil {
		return fmt.Errorf("写入新周期边界: %w", err)
	}

	n.CycleStart, n.CycleEnd = start, end
	n.ExceedHandledAt, n.WarnHandledAt = nil, nil

	msg := fmt.Sprintf("节点「%s」%s：上一周期用量 %s（入 %s / 出 %s），已归档并清零，新周期至 %s",
		n.Name, reason, humanBytes(billed),
		humanBytes(usage.RxBytes), humanBytes(usage.TxBytes),
		end.In(loc).Format("2006-01-02 15:04"))

	e.log.Info("账单周期已滚动", "node", n.Name, "billed", billed, "new_end", end)

	id := n.ID
	e.st.AddEvent(ctx, &id, store.EventCycleReset, store.LevelInfo, msg)
	e.notifier.Send(notify.Message{
		Level: store.LevelInfo, Title: "流量已清零", Body: msg,
		NodeID: n.ID, NodeName: n.Name,
	})

	// 上个周期因为超额被关停的节点，新周期开始时恢复可用状态。
	// 注意：面板只能把状态标回来，机器如果被关机了得用户自己开——
	// 我们没有带外管理权限。事件里会写清楚这一点。
	if n.AutoRecoverOnReset && (n.Status == store.StatusExceeded || n.Status == store.StatusStopped) {
		wasStopped := n.Status == store.StatusStopped
		if err := e.st.SetNodeStatus(ctx, n.ID, store.StatusOffline); err != nil {
			e.log.Warn("恢复节点状态失败", "node", n.Name, "err", err)
		} else {
			n.Status = store.StatusOffline
			note := fmt.Sprintf("节点「%s」已随新周期解除超额限制", n.Name)
			if wasStopped {
				note += "。该机器此前被面板关机，需要你在服务商后台手动开机后探针才会重新上线"
			}
			e.st.AddEvent(ctx, &id, store.EventCycleReset, store.LevelInfo, note)
		}
	}

	// 主节点流量恢复了，把域名切回去
	if err := e.fo.ReconcileNode(ctx, n.ID); err != nil {
		e.log.Warn("清零后的 DNS 对账失败", "node", n.Name, "err", err)
	}

	return nil
}

// pruneIfDue 定期清理小时曲线和事件日志，别让 SQLite 无限长大。
func (e *Engine) pruneIfDue(ctx context.Context, cfg store.Settings) {
	if time.Since(e.lastPrune) < pruneInterval {
		return
	}
	e.lastPrune = time.Now()

	days := cfg.HourlyRetainDays
	if days <= 0 {
		days = 180
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	if n, err := e.st.PruneHourly(ctx, cutoff); err != nil {
		e.log.Warn("清理历史流量曲线失败", "err", err)
	} else if n > 0 {
		e.log.Info("已清理过期流量曲线", "rows", n, "before", cutoff.Format("2006-01-02"))
	}

	if n, err := e.st.PruneEvents(ctx, 20000); err != nil {
		e.log.Warn("清理历史事件失败", "err", err)
	} else if n > 0 {
		e.log.Info("已清理过期事件", "rows", n)
	}
}
