// Package ingest 把探针上报的网卡累计计数器转换成流量增量，并记进面板账本。
//
// 这是整个系统里最要紧的一段逻辑。设计目标：节点机器怎么重启，账本都不能丢、不能重。
// 做法是探针只报"网卡自开机以来的累计值"这种幂等的原始数据，
// 由面板对比上一次快照算增量。判重启靠 boot_id，不靠探针自己记账。
package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// 增量的来源，写进事件日志便于排查。
const (
	ReasonFirst        = "first"         // 该节点的首次上报，只建基线不计流量
	ReasonNormal       = "normal"        // 正常递增
	ReasonReboot       = "reboot"        // boot_id 变了：机器重启，计数器已归零
	ReasonCounterReset = "counter_reset" // boot_id 没变但计数器回退：网卡被重建之类
	ReasonIfaceChanged = "iface_changed" // 统计的网卡换了，重新建基线
)

// Delta 是一次上报算出的增量。
type Delta struct {
	Rx     int64
	Tx     int64
	Reason string
}

// ComputeDelta 对比上一次快照，算出本次应计入账本的流量增量。
//
// 各种情况的处理：
//
//	prev == nil        首次上报。计数器里存的是探针装上去之前就积累的历史流量，
//	                   全部计入会凭空多算，所以只建基线，增量为 0。
//	boot_id 变化        机器重启过，计数器从 0 重新开始。当前值就是重启以来的流量，
//	                   全部计入。中间丢失的只有"重启前最后一次上报到断电"那一小段。
//	网卡名变化           换了统计口径，新旧计数器不可比。重新建基线，增量为 0，
//	                   否则会把新网卡的历史累计值重复计入。
//	计数器回退           boot_id 没变但数字变小了（网卡被 down/up 重建等）。
//	                   按重置处理，当前值全部计入。
//	其余                 正常做差。
func ComputeDelta(prev *store.Counter, r protocol.Report) Delta {
	rx, tx := r.Rx, r.Tx
	if rx < 0 {
		rx = 0
	}
	if tx < 0 {
		tx = 0
	}

	switch {
	case prev == nil:
		return Delta{Rx: 0, Tx: 0, Reason: ReasonFirst}

	case prev.Iface != "" && r.Iface != "" && prev.Iface != r.Iface:
		return Delta{Rx: 0, Tx: 0, Reason: ReasonIfaceChanged}

	case prev.BootID != r.BootID:
		return Delta{Rx: rx, Tx: tx, Reason: ReasonReboot}

	case rx < prev.LastRx || tx < prev.LastTx:
		return Delta{Rx: rx, Tx: tx, Reason: ReasonCounterReset}

	default:
		return Delta{Rx: rx - prev.LastRx, Tx: tx - prev.LastTx, Reason: ReasonNormal}
	}
}

// Result 是一次入账的结果。
type Result struct {
	Delta Delta
	Usage *store.Usage
}

// Ingestor 把上报写进账本。
type Ingestor struct {
	st *store.Store
	// now 可在测试里替换。
	now func() time.Time
}

func New(st *store.Store) *Ingestor {
	return &Ingestor{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// Apply 处理一次上报：算增量 → 写计数器、周期累计、小时聚合、心跳时间。
// 全程在一个事务里，保证并发上报不会重复计账。
func (i *Ingestor) Apply(ctx context.Context, node *store.Node, r protocol.Report) (*Result, error) {
	now := i.now()
	res := &Result{}

	err := i.st.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		prev, err := tx.GetCounter(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("读取上次计数器: %w", err)
		}

		d := ComputeDelta(prev, r)
		res.Delta = d

		if err := tx.SaveCounter(ctx, &store.Counter{
			NodeID:    node.ID,
			BootID:    r.BootID,
			Iface:     r.Iface,
			LastRx:    r.Rx,
			LastTx:    r.Tx,
			UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("保存计数器: %w", err)
		}

		// 增量为 0 时也要走一遍 AddUsage，这样 node_usage 行一定存在，
		// 前端不用处理"记录缺失"的分支。
		usage, err := tx.AddUsage(ctx, node.ID, d.Rx, d.Tx, node.CycleStart, now)
		if err != nil {
			return fmt.Errorf("累加周期用量: %w", err)
		}
		res.Usage = usage

		if d.Rx > 0 || d.Tx > 0 {
			// 小时桶用面板时间，不用探针时间——节点时钟不准不该污染曲线。
			if err := tx.AddHourly(ctx, node.ID, now, d.Rx, d.Tx); err != nil {
				return fmt.Errorf("写入小时聚合: %w", err)
			}
		}

		// 转发计数走另一套表，只做分账展示，不影响上面的账本和配额判定。
		// 放在同一个事务里是为了一次上报只开一个写事务。
		if len(r.Forwards) > 0 {
			if err := applyForward(ctx, tx, node, r.Forwards, now); err != nil {
				return err
			}
		}

		return tx.TouchNode(ctx, node.ID, now)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TouchOnly 只更新心跳时间，不动流量账本（用于 pong 之类的保活帧）。
func (i *Ingestor) TouchOnly(ctx context.Context, nodeID int64) error {
	return i.st.TouchNode(ctx, nodeID, i.now())
}
