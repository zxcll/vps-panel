package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// ForwardDelta 是一跳转发在一次上报里算出的增量。
type ForwardDelta struct {
	Up     int64
	Down   int64
	Reason string
}

// ComputeForwardDelta 算一跳转发本次应计入的增量。
//
// 和网卡计数器那套（ComputeDelta）是同构的，只是把 boot_id 换成了 epoch：
// 探针的转发状态文件丢失或重建时会换一个新 epoch，面板据此判断"计数器已归零"。
//
//	prev == nil     首次上报，只建基线不计流量
//	epoch 变化       探针侧计数从头开始了，当前值全部计入
//	计数回退         epoch 没变但数字变小（探针刚好在两次上报之间重建了监听器），
//	                 按重置处理，当前值全部计入
//	其余             正常做差
//
// 由此得到和节点账本一样的三条保证：重复上报不重复计账（cur == last → 0）、
// 探针重启不丢账（epoch 还在就接着算）、面板重启不丢账（基线在库里）。
func ComputeForwardDelta(prev *store.ForwardCounter, s protocol.ForwardSample) ForwardDelta {
	up, down := s.BytesUp, s.BytesDown
	if up < 0 {
		up = 0
	}
	if down < 0 {
		down = 0
	}

	switch {
	case prev == nil:
		return ForwardDelta{Reason: ReasonFirst}

	case prev.Epoch != s.Epoch:
		return ForwardDelta{Up: up, Down: down, Reason: ReasonReboot}

	case up < prev.LastUp || down < prev.LastDown:
		return ForwardDelta{Up: up, Down: down, Reason: ReasonCounterReset}

	default:
		return ForwardDelta{Up: up - prev.LastUp, Down: down - prev.LastDown, Reason: ReasonNormal}
	}
}

// applyForward 把一次上报里的转发计数写进转发账本。
//
// 必须和网卡账本共用同一个事务：一次上报开两个写事务的话，
// 在 _txlock=immediate 下两者会互相阻塞（见 store.Open 里的说明）。
//
// 重要：这里只写 forward_* 三张表，绝不碰 node_usage。
// 中转流量在网卡上进出各走一遍，已经被节点账本算进去了，
// 再加一遍就是重复计费，会误触发超额关机。
func applyForward(ctx context.Context, tx *store.Tx, node *store.Node, samples []protocol.ForwardSample, now time.Time) error {
	for _, s := range samples {
		if s.HopID <= 0 {
			continue
		}
		// 探针会继续上报刚被删掉的跳（把最后一段流量送回来），
		// 认不出来的直接跳过 —— 不然外键会让整个上报事务回滚，
		// 连带把这台机器的网卡账本也一起丢掉。
		exists, err := tx.HopExists(ctx, s.HopID)
		if err != nil {
			return fmt.Errorf("检查转发跳 %d: %w", s.HopID, err)
		}
		if !exists {
			continue
		}

		prev, err := tx.GetForwardCounter(ctx, s.HopID)
		if err != nil {
			return fmt.Errorf("读取转发计数 %d: %w", s.HopID, err)
		}
		d := ComputeForwardDelta(prev, s)

		if err := tx.SaveForwardCounter(ctx, &store.ForwardCounter{
			HopID:     s.HopID,
			Epoch:     s.Epoch,
			LastUp:    s.BytesUp,
			LastDown:  s.BytesDown,
			UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("保存转发计数 %d: %w", s.HopID, err)
		}

		// 增量为 0 也要写一遍，保证 forward_usage 行一定存在，
		// 前端不用处理"记录缺失"的分支。
		if err := tx.AddForwardUsage(ctx, s.HopID, d.Up, d.Down, node.CycleStart, now); err != nil {
			return fmt.Errorf("累加转发用量 %d: %w", s.HopID, err)
		}

		if d.Up > 0 || d.Down > 0 {
			if err := tx.AddForwardHourly(ctx, s.HopID, now, d.Up, d.Down); err != nil {
				return fmt.Errorf("写入转发小时聚合 %d: %w", s.HopID, err)
			}
		}
	}
	return nil
}
