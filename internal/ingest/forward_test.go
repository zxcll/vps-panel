package ingest

import (
	"context"
	"testing"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

func TestComputeForwardDelta(t *testing.T) {
	prev := &store.ForwardCounter{Epoch: "e1", LastUp: 1000, LastDown: 2000}

	cases := []struct {
		name       string
		prev       *store.ForwardCounter
		sample     protocol.ForwardSample
		wantUp     int64
		wantDown   int64
		wantReason string
	}{
		{
			// 首次上报只建基线。计数里可能含探针装上之前就在跑的流量，
			// 全部计入会凭空多算。
			"首次上报只建基线",
			nil,
			protocol.ForwardSample{Epoch: "e1", BytesUp: 5000, BytesDown: 6000},
			0, 0, ReasonFirst,
		},
		{
			"正常递增",
			prev,
			protocol.ForwardSample{Epoch: "e1", BytesUp: 1500, BytesDown: 2300},
			500, 300, ReasonNormal,
		},
		{
			// 同一份报文重投，做差得 0，天然幂等。
			"重复上报同一报文",
			prev,
			protocol.ForwardSample{Epoch: "e1", BytesUp: 1000, BytesDown: 2000},
			0, 0, ReasonNormal,
		},
		{
			// 探针状态文件丢了，逻辑计数从 0 重来，所以换了 epoch。
			// 当前值就是重来之后的全部流量。
			"epoch 变化按归零处理",
			prev,
			protocol.ForwardSample{Epoch: "e2", BytesUp: 300, BytesDown: 400},
			300, 400, ReasonReboot,
		},
		{
			"计数回退按重置处理",
			prev,
			protocol.ForwardSample{Epoch: "e1", BytesUp: 100, BytesDown: 2500},
			100, 2500, ReasonCounterReset,
		},
		{
			// 负数先钳到 0；钳完之后上行比基线小，判定为重置，
			// 于是两个方向都按当前值全量计入 —— 和 ComputeDelta 的保守取舍一致：
			// 任一方向回退就认为整个计数器不可信了。
			"负数钳到 0 并触发重置判定",
			prev,
			protocol.ForwardSample{Epoch: "e1", BytesUp: -5, BytesDown: 2000},
			0, 2000, ReasonCounterReset,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ComputeForwardDelta(tc.prev, tc.sample)
			if d.Up != tc.wantUp || d.Down != tc.wantDown {
				t.Errorf("增量 = %d/%d，期望 %d/%d", d.Up, d.Down, tc.wantUp, tc.wantDown)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("原因 = %q，期望 %q", d.Reason, tc.wantReason)
			}
			if d.Up < 0 || d.Down < 0 {
				t.Errorf("增量不能为负：%+v", d)
			}
		})
	}
}

// 建一条单跳规则，返回它的 hop_id。
func newTestHop(t *testing.T, st *store.Store, nodeID int64, listenPort int) int64 {
	t.Helper()
	r := &store.ForwardRule{
		Name: "测试规则", Proto: store.ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{
			{NodeID: nodeID, ListenPort: listenPort, Mode: store.ForwardModeKernel},
		},
	}
	if err := st.CreateForwardRule(context.Background(), r); err != nil {
		t.Fatalf("建转发规则失败: %v", err)
	}
	return r.Hops[0].ID
}

// 这条是本方案最核心的一条铁律：转发计数一个字节都不能进节点账本。
// 转发流量在网卡上进出各走一遍，已经被网卡计数器算进去了。
func TestForwardCountersNeverTouchNodeLedger(t *testing.T) {
	st := newTestStore(t)
	node := newTestNode(t, st)
	ctx := context.Background()
	hopID := newTestHop(t, st, node.ID, 8443)
	ing := New(st)

	// 先建基线。
	if _, err := ing.Apply(ctx, node, protocol.Report{
		BootID: "boot-a", Iface: "eth0", Rx: 1000, Tx: 2000,
		Forwards: []protocol.ForwardSample{{HopID: hopID, Epoch: "e1"}},
	}); err != nil {
		t.Fatalf("首次上报失败: %v", err)
	}

	// 第二次：网卡涨了 500/800，转发涨了 300/400。
	if _, err := ing.Apply(ctx, node, protocol.Report{
		BootID: "boot-a", Iface: "eth0", Rx: 1500, Tx: 2800,
		Forwards: []protocol.ForwardSample{{HopID: hopID, Epoch: "e1", BytesUp: 300, BytesDown: 400}},
	}); err != nil {
		t.Fatalf("第二次上报失败: %v", err)
	}

	usage, err := st.GetUsage(ctx, node.ID)
	if err != nil {
		t.Fatalf("读节点账本失败: %v", err)
	}
	// 节点账本必须严格等于网卡增量，不多一个字节。
	if usage.RxBytes != 500 || usage.TxBytes != 800 {
		t.Fatalf("节点账本被转发计数污染了：rx=%d tx=%d，期望 500/800", usage.RxBytes, usage.TxBytes)
	}

	fwd, err := st.AllForwardUsage(ctx)
	if err != nil {
		t.Fatalf("读转发账本失败: %v", err)
	}
	if fwd[hopID] == nil || fwd[hopID].UpBytes != 300 || fwd[hopID].DownBytes != 400 {
		t.Errorf("转发账本记错了：%+v", fwd[hopID])
	}
}

func TestForwardIngestIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	node := newTestNode(t, st)
	ctx := context.Background()
	hopID := newTestHop(t, st, node.ID, 8443)
	ing := New(st)

	rep := protocol.Report{
		BootID: "boot-a", Iface: "eth0", Rx: 1000, Tx: 2000,
		Forwards: []protocol.ForwardSample{{HopID: hopID, Epoch: "e1", BytesUp: 100, BytesDown: 200}},
	}
	// 首次建基线。
	if _, err := ing.Apply(ctx, node, rep); err != nil {
		t.Fatalf("首次上报失败: %v", err)
	}
	// 同一份报文再投两次，账本不该动。
	for range 2 {
		if _, err := ing.Apply(ctx, node, rep); err != nil {
			t.Fatalf("重投失败: %v", err)
		}
	}
	fwd, _ := st.AllForwardUsage(ctx)
	if fwd[hopID].UpBytes != 0 || fwd[hopID].DownBytes != 0 {
		t.Errorf("重复上报被重复计账了：%+v", fwd[hopID])
	}
}

// 探针侧 apply 会清零 nft 计数，探针靠逻辑累计量把它抹平；
// 状态文件真丢了才换 epoch。这条验证 epoch 变化时面板按"归零"处理。
func TestForwardEpochChangeCountsFullValue(t *testing.T) {
	st := newTestStore(t)
	node := newTestNode(t, st)
	ctx := context.Background()
	hopID := newTestHop(t, st, node.ID, 8443)
	ing := New(st)

	steps := []struct {
		epoch    string
		up, down int64
	}{
		{"e1", 0, 0},       // 建基线
		{"e1", 1000, 2000}, // +1000/+2000
		{"e2", 300, 400},   // 换 epoch：+300/+400（不是负数，也不是 300-1000）
		{"e2", 500, 600},   // +200/+200
	}
	for i, s := range steps {
		if _, err := ing.Apply(ctx, node, protocol.Report{
			BootID: "boot-a", Iface: "eth0", Rx: int64(1000 + i), Tx: int64(2000 + i),
			Forwards: []protocol.ForwardSample{{HopID: hopID, Epoch: s.epoch, BytesUp: s.up, BytesDown: s.down}},
		}); err != nil {
			t.Fatalf("第 %d 次上报失败: %v", i+1, err)
		}
	}

	fwd, _ := st.AllForwardUsage(ctx)
	// 1000 + 300 + 200 = 1500，2000 + 400 + 200 = 2600
	if fwd[hopID].UpBytes != 1500 || fwd[hopID].DownBytes != 2600 {
		t.Errorf("累计 = %d/%d，期望 1500/2600", fwd[hopID].UpBytes, fwd[hopID].DownBytes)
	}
}

// 探针会继续上报刚被删掉的跳。面板必须静默跳过，
// 绝不能因为外键报错把整个上报事务回滚 —— 那会连网卡账本一起丢。
func TestUnknownHopDoesNotBreakReport(t *testing.T) {
	st := newTestStore(t)
	node := newTestNode(t, st)
	ctx := context.Background()
	ing := New(st)

	if _, err := ing.Apply(ctx, node, protocol.Report{
		BootID: "boot-a", Iface: "eth0", Rx: 1000, Tx: 2000,
	}); err != nil {
		t.Fatalf("首次上报失败: %v", err)
	}

	_, err := ing.Apply(ctx, node, protocol.Report{
		BootID: "boot-a", Iface: "eth0", Rx: 1500, Tx: 2800,
		Forwards: []protocol.ForwardSample{{HopID: 99999, Epoch: "e1", BytesUp: 100, BytesDown: 200}},
	})
	if err != nil {
		t.Fatalf("上报里含已删除的跳时不该失败: %v", err)
	}

	usage, _ := st.GetUsage(ctx, node.ID)
	if usage.RxBytes != 500 || usage.TxBytes != 800 {
		t.Errorf("网卡账本应照常入账：rx=%d tx=%d，期望 500/800", usage.RxBytes, usage.TxBytes)
	}
}

// 老探针不会带 forwards 字段，面板要能正常降级。
func TestReportWithoutForwardsStillWorks(t *testing.T) {
	st := newTestStore(t)
	node := newTestNode(t, st)
	ctx := context.Background()
	ing := New(st)

	if _, err := ing.Apply(ctx, node, protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 1000, Tx: 2000}); err != nil {
		t.Fatalf("首次上报失败: %v", err)
	}
	res, err := ing.Apply(ctx, node, protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 1500, Tx: 2800})
	if err != nil {
		t.Fatalf("老探针格式的上报失败: %v", err)
	}
	if res.Delta.Rx != 500 || res.Delta.Tx != 800 {
		t.Errorf("增量 = %d/%d，期望 500/800", res.Delta.Rx, res.Delta.Tx)
	}
}
