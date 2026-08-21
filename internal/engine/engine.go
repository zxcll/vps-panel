// Package engine 是面板的后台调度中枢。
//
// 它把几件事串起来：
//   - 账单周期到点了就归档 + 清零
//   - 流量到达预警线/配额上限时，执行用户配置的动作（关机、停服务、切 DNS）
//   - 节点上线/离线的状态翻转与事件记录
//   - 驱动 failover 做域名对账
//
// 判定入口有两个：定时轮询（兜底）和每次探针上报后的即时检查（快速响应）。
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/failover"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

// TickInterval 是调度器的轮询周期。
// 30 秒足够及时，又不会把 SQLite 和 TCP 拨测压垮。
const TickInterval = 30 * time.Second

// pruneInterval 是清理历史数据的周期。
const pruneInterval = 6 * time.Hour

type Engine struct {
	st       *store.Store
	exec     *action.Executor
	fo       *failover.Manager
	notifier *notify.Notifier
	log      *slog.Logger

	// mu 保护节点级的动作执行，避免定时轮询和即时检查同时对一台机器下关机指令。
	mu      sync.Mutex
	working map[int64]bool

	lastPrune time.Time
}

func New(st *store.Store, exec *action.Executor, fo *failover.Manager, n *notify.Notifier, log *slog.Logger) *Engine {
	return &Engine{
		st: st, exec: exec, fo: fo, notifier: n, log: log,
		working: map[int64]bool{},
	}
}

// Run 启动调度循环，直到 ctx 取消。
func (e *Engine) Run(ctx context.Context) {
	// 启动时先跑一轮，把面板停机期间积压的周期滚动、状态变化补上
	e.Tick(ctx)

	t := time.NewTicker(TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			e.log.Info("调度器已停止")
			return
		case <-t.C:
			e.Tick(ctx)
		}
	}
}

// Tick 执行一轮完整的检查。
func (e *Engine) Tick(ctx context.Context) {
	cfg, err := e.st.LoadSettings(ctx)
	if err != nil {
		e.log.Error("读取运行参数失败", "err", err)
		return
	}

	// 一次性采集所有节点状态（含 TCP 拨测），后面几步共用
	states, err := e.fo.Snapshot(ctx)
	if err != nil {
		e.log.Error("采集节点状态失败", "err", err)
		return
	}

	for id, s := range states {
		e.fo.MarkAlive(id, s.Alive, cfg.FailThreshold)
		e.syncLiveness(ctx, s, cfg)
	}

	for _, s := range states {
		if err := e.rollCycleIfDue(ctx, s.Node); err != nil {
			e.log.Error("滚动账单周期失败", "node", s.Node.Name, "err", err)
		}
	}

	// 周期可能刚刚滚动过，重新读一遍用量再判配额
	for _, s := range states {
		node, err := e.st.GetNode(ctx, s.Node.ID)
		if err != nil {
			continue
		}
		usage, err := e.st.GetUsage(ctx, node.ID)
		if err != nil {
			continue
		}
		e.CheckQuota(ctx, node, usage)
	}

	// 重新采一次快照：上面的动作可能改了节点状态（超额/关停），
	// 用旧快照选主会把已经关掉的机器又挑回来。
	states2, err := e.fo.Snapshot(ctx)
	if err != nil {
		e.log.Error("对账前重新采集状态失败", "err", err)
		states2 = states
	}
	if err := e.fo.ReconcileWith(ctx, states2, cfg); err != nil {
		e.log.Warn("DNS 对账过程中出现错误", "err", err)
	}

	e.pruneIfDue(ctx, cfg)
}

// syncLiveness 维护节点的在线/离线状态，只在状态翻转时写事件，避免刷屏。
func (e *Engine) syncLiveness(ctx context.Context, s *failover.NodeState, cfg store.Settings) {
	n := s.Node

	// 被超额/关停逻辑接管的节点，在线状态不参与覆盖
	if n.Status == store.StatusExceeded || n.Status == store.StatusStopped {
		return
	}

	switch {
	case s.Alive && n.Status != store.StatusOnline:
		if err := e.st.SetNodeStatus(ctx, n.ID, store.StatusOnline); err != nil {
			e.log.Warn("更新节点状态失败", "node", n.Name, "err", err)
			return
		}
		// unknown → online 是新节点第一次连上，不算"恢复"，不打扰用户
		if n.Status == store.StatusOffline {
			id := n.ID
			e.st.AddEvent(ctx, &id, store.EventNodeOnline, store.LevelInfo,
				fmt.Sprintf("节点「%s」已恢复在线", n.Name))
			e.notifier.Send(notify.Message{
				Level: store.LevelInfo, Title: "节点已恢复",
				Body: fmt.Sprintf("节点「%s」重新上线", n.Name), NodeID: n.ID, NodeName: n.Name,
			})
		}

	case !s.Alive && n.Status != store.StatusOffline:
		// 连续失败达到阈值才认账，一次抖动不写事件
		if e.fo.FailCount(n.ID) < maxInt(cfg.FailThreshold, 1) {
			return
		}
		if err := e.st.SetNodeStatus(ctx, n.ID, store.StatusOffline); err != nil {
			e.log.Warn("更新节点状态失败", "node", n.Name, "err", err)
			return
		}
		id := n.ID
		reason := s.Reason
		if reason == "" {
			reason = "探针失联"
		}
		e.st.AddEvent(ctx, &id, store.EventNodeOffline, store.LevelWarn,
			fmt.Sprintf("节点「%s」已离线：%s", n.Name, reason))
		e.notifier.Send(notify.Message{
			Level: store.LevelWarn, Title: "节点离线",
			Body: fmt.Sprintf("节点「%s」离线：%s", n.Name, reason), NodeID: n.ID, NodeName: n.Name,
		})
	}
}

// CheckQuota 判断节点是否触发预警或超额，并执行相应动作。
// 探针每次上报后也会调用它，好让流量跑满能在几十秒内被处理掉。
func (e *Engine) CheckQuota(ctx context.Context, n *store.Node, u *store.Usage) {
	if n.QuotaBytes <= 0 {
		return
	}

	st := quota.EvaluateNode(n, u)

	switch {
	case st.Exceeded && n.ExceedHandledAt == nil:
		e.handleExceed(ctx, n, st)
	case st.Warning && n.WarnHandledAt == nil:
		e.handleWarn(ctx, n, st)
	}
}

// lock 保证同一个节点同时只有一条动作在跑。
func (e *Engine) lock(nodeID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.working[nodeID] {
		return false
	}
	e.working[nodeID] = true
	return true
}

func (e *Engine) unlock(nodeID int64) {
	e.mu.Lock()
	delete(e.working, nodeID)
	e.mu.Unlock()
}

func (e *Engine) handleWarn(ctx context.Context, n *store.Node, st quota.Status) {
	if !e.lock(n.ID) {
		return
	}
	defer e.unlock(n.ID)

	// 先落"已处理"标记再做别的，防止下一轮重复触发
	if err := e.st.MarkWarnHandled(ctx, n.ID, time.Now().UTC()); err != nil {
		e.log.Error("记录预警标记失败", "node", n.Name, "err", err)
		return
	}

	msg := fmt.Sprintf("节点「%s」流量已用 %s / %s（%.1f%%），达到 %d%% 预警线",
		n.Name, humanBytes(st.Billed), humanBytes(st.Quota), st.Percent, n.WarnPercent)
	e.log.Warn("流量预警", "node", n.Name, "percent", st.Percent)

	id := n.ID
	e.st.AddEvent(ctx, &id, store.EventTrafficWarn, store.LevelWarn, msg)
	e.notifier.Send(notify.Message{
		Level: store.LevelWarn, Title: "流量预警", Body: msg,
		NodeID: n.ID, NodeName: n.Name,
	})

	// 开了"预警即切换"的记录，这时候就把解析挪走
	if err := e.fo.ReconcileNode(ctx, n.ID); err != nil {
		e.log.Warn("预警触发的 DNS 对账失败", "node", n.Name, "err", err)
	}
}

func (e *Engine) handleExceed(ctx context.Context, n *store.Node, st quota.Status) {
	if !e.lock(n.ID) {
		return
	}
	defer e.unlock(n.ID)

	now := time.Now().UTC()
	// 同样先落标记：关机是不可逆操作，宁可漏执行也不能重复执行
	if err := e.st.MarkExceedHandled(ctx, n.ID, now); err != nil {
		e.log.Error("记录超额标记失败", "node", n.Name, "err", err)
		return
	}
	if err := e.st.SetNodeStatus(ctx, n.ID, store.StatusExceeded); err != nil {
		e.log.Warn("更新节点状态失败", "node", n.Name, "err", err)
	}

	msg := fmt.Sprintf("节点「%s」流量已耗尽：%s / %s（口径：%s）",
		n.Name, humanBytes(st.Billed), humanBytes(st.Quota), modeLabel(n.BillingMode))
	e.log.Warn("流量超额", "node", n.Name, "billed", st.Billed, "quota", st.Quota)

	id := n.ID
	e.st.AddEvent(ctx, &id, store.EventTrafficExceed, store.LevelError, msg)
	e.notifier.Send(notify.Message{
		Level: store.LevelError, Title: "流量已耗尽", Body: msg,
		NodeID: n.ID, NodeName: n.Name,
	})

	// 先把域名切走，再动机器 —— 顺序反了会有一段时间域名指向一台正在关机的机器
	if err := e.fo.ReconcileNode(ctx, n.ID); err != nil {
		e.log.Warn("超额触发的 DNS 对账失败", "node", n.Name, "err", err)
	}

	e.runExceedAction(ctx, n, "流量耗尽自动处置")
}

// runExceedAction 执行超额动作并记录结果。
func (e *Engine) runExceedAction(ctx context.Context, n *store.Node, reason string) {
	if n.ActionOnExceed == store.ActionNone || n.ActionOnExceed == store.ActionDNSOnly || n.ActionOnExceed == "" {
		return
	}

	// 关机/停服务这类动作单独给一个较宽的超时：SSH 建连本身就可能要十几秒
	actCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	id := n.ID
	outcome, err := e.exec.RunExceedAction(actCtx, n, reason)
	if err != nil {
		msg := fmt.Sprintf("节点「%s」执行超额动作（%s）失败：%v",
			n.Name, actionLabel(n.ActionOnExceed), err)
		e.log.Error("超额动作执行失败", "node", n.Name, "action", n.ActionOnExceed, "err", err)
		e.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelError, msg)
		e.notifier.Send(notify.Message{
			Level: store.LevelError, Title: "超额处置失败", Body: msg,
			NodeID: n.ID, NodeName: n.Name,
		})
		return
	}
	if outcome == nil {
		return
	}

	msg := fmt.Sprintf("节点「%s」已执行超额动作：%s（通道：%s）",
		n.Name, actionLabel(n.ActionOnExceed), channelLabel(outcome.Via))
	if outcome.Output != "" {
		msg += "\n输出：" + truncate(outcome.Output, 500)
	}
	e.log.Info("超额动作执行完成", "node", n.Name, "action", n.ActionOnExceed, "via", outcome.Via)
	e.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelWarn, msg)
	e.notifier.Send(notify.Message{
		Level: store.LevelWarn, Title: "已执行超额处置", Body: msg,
		NodeID: n.ID, NodeName: n.Name,
	})

	if action.StopsMachine(n.ActionOnExceed) {
		if err := e.st.SetNodeStatus(ctx, n.ID, store.StatusStopped); err != nil {
			e.log.Warn("更新节点状态失败", "node", n.Name, "err", err)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
