package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

func TestComputeDelta(t *testing.T) {
	prev := &store.Counter{BootID: "boot-a", Iface: "eth0", LastRx: 1000, LastTx: 2000}

	cases := []struct {
		name       string
		prev       *store.Counter
		report     protocol.Report
		wantRx     int64
		wantTx     int64
		wantReason string
	}{
		{
			name:       "首次上报只建基线，不计流量",
			prev:       nil,
			report:     protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 9_000_000, Tx: 8_000_000},
			wantRx:     0,
			wantTx:     0,
			wantReason: ReasonFirst,
		},
		{
			name:       "正常递增",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 1500, Tx: 2500},
			wantRx:     500,
			wantTx:     500,
			wantReason: ReasonNormal,
		},
		{
			name:       "机器重启：boot_id 变化，当前值全部计入",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-b", Iface: "eth0", Rx: 300, Tx: 400},
			wantRx:     300,
			wantTx:     400,
			wantReason: ReasonReboot,
		},
		{
			name:       "计数器回退：boot_id 未变但数字变小",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 100, Tx: 2500},
			wantRx:     100,
			wantTx:     2500,
			wantReason: ReasonCounterReset,
		},
		{
			name:       "换网卡：重新建基线，不计流量",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-a", Iface: "ens3", Rx: 5_000_000, Tx: 6_000_000},
			wantRx:     0,
			wantTx:     0,
			wantReason: ReasonIfaceChanged,
		},
		{
			name:       "增量为零",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-a", Iface: "eth0", Rx: 1000, Tx: 2000},
			wantRx:     0,
			wantTx:     0,
			wantReason: ReasonNormal,
		},
		{
			name:       "负数计数器按 0 处理",
			prev:       prev,
			report:     protocol.Report{BootID: "boot-b", Iface: "eth0", Rx: -5, Tx: 100},
			wantRx:     0,
			wantTx:     100,
			wantReason: ReasonReboot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ComputeDelta(tc.prev, tc.report)
			if d.Rx != tc.wantRx || d.Tx != tc.wantTx {
				t.Errorf("增量 = (%d, %d)，期望 (%d, %d)", d.Rx, d.Tx, tc.wantRx, tc.wantTx)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("原因 = %q，期望 %q", d.Reason, tc.wantReason)
			}
			if d.Rx < 0 || d.Tx < 0 {
				t.Errorf("增量不该为负：(%d, %d)", d.Rx, d.Tx)
			}
		})
	}
}

// --- 端到端：跑真实 SQLite ---

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestNode(t *testing.T, st *store.Store) *store.Node {
	t.Helper()
	n := &store.Node{
		Name:         "测试节点",
		Secret:       "secret-" + t.Name(),
		BillingMode:  store.BillingSum,
		TrafficRatio: 1,
		WarnPercent:  90,
		ResetDay:     1,
		ResetTZ:      "UTC",
		CycleStart:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		CycleEnd:     time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Status:       store.StatusUnknown,
		Enabled:      true,
		SSHPort:      22,
	}
	if err := st.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("创建测试节点: %v", err)
	}
	return n
}

// 这是本项目最关键的一条验收：节点机器重启后，面板账本必须继续累加而不是归零。
func TestApplySurvivesReboot(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	node := newTestNode(t, st)
	ing := New(st)

	// 第一次上报：机器已经跑了一阵，计数器不为零。只建基线。
	r1, err := ing.Apply(ctx, node, protocol.Report{BootID: "boot-1", Iface: "eth0", Rx: 5_000_000, Tx: 7_000_000})
	if err != nil {
		t.Fatalf("首次上报: %v", err)
	}
	if r1.Usage.RxBytes != 0 || r1.Usage.TxBytes != 0 {
		t.Fatalf("首次上报应只建基线，实际累计 rx=%d tx=%d", r1.Usage.RxBytes, r1.Usage.TxBytes)
	}

	// 正常跑了一段
	r2, _ := ing.Apply(ctx, node, protocol.Report{BootID: "boot-1", Iface: "eth0", Rx: 6_000_000, Tx: 9_000_000})
	if r2.Usage.RxBytes != 1_000_000 || r2.Usage.TxBytes != 2_000_000 {
		t.Fatalf("正常累加错误：rx=%d tx=%d，期望 1000000/2000000", r2.Usage.RxBytes, r2.Usage.TxBytes)
	}

	// 机器重启：boot_id 变化，计数器从 0 开始
	r3, _ := ing.Apply(ctx, node, protocol.Report{BootID: "boot-2", Iface: "eth0", Rx: 300_000, Tx: 400_000})
	if r3.Delta.Reason != ReasonReboot {
		t.Errorf("应识别为重启，实际 %q", r3.Delta.Reason)
	}
	if r3.Usage.RxBytes != 1_300_000 || r3.Usage.TxBytes != 2_400_000 {
		t.Fatalf("重启后账本被清零了！rx=%d tx=%d，期望 1300000/2400000", r3.Usage.RxBytes, r3.Usage.TxBytes)
	}

	// 重启后继续跑
	r4, _ := ing.Apply(ctx, node, protocol.Report{BootID: "boot-2", Iface: "eth0", Rx: 500_000, Tx: 900_000})
	if r4.Usage.RxBytes != 1_500_000 || r4.Usage.TxBytes != 2_900_000 {
		t.Fatalf("重启后继续累加错误：rx=%d tx=%d，期望 1500000/2900000", r4.Usage.RxBytes, r4.Usage.TxBytes)
	}
}

// 重复收到同一份报文（探针重发/网络重投）不应重复计账。
func TestApplyIsIdempotentForRepeatedReport(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	node := newTestNode(t, st)
	ing := New(st)

	rep := protocol.Report{BootID: "boot-1", Iface: "eth0", Rx: 1000, Tx: 2000}
	if _, err := ing.Apply(ctx, node, rep); err != nil {
		t.Fatal(err)
	}
	rep2 := protocol.Report{BootID: "boot-1", Iface: "eth0", Rx: 5000, Tx: 6000}
	r, _ := ing.Apply(ctx, node, rep2)
	first := *r.Usage

	// 同一份报文再来三次
	for i := 0; i < 3; i++ {
		r, _ = ing.Apply(ctx, node, rep2)
	}
	if r.Usage.RxBytes != first.RxBytes || r.Usage.TxBytes != first.TxBytes {
		t.Errorf("重复报文被重复计账：%d/%d → %d/%d",
			first.RxBytes, first.TxBytes, r.Usage.RxBytes, r.Usage.TxBytes)
	}
}

// 面板自己重启（进程重启，DB 保留）后账本要接得上。
func TestApplySurvivesPanelRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "panel.db")

	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	node := newTestNode(t, st1)
	ing1 := New(st1)
	if _, err := ing1.Apply(ctx, node, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 1000, Tx: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := ing1.Apply(ctx, node, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 3000, Tx: 3000}); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	// 面板重启
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	node2, err := st2.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	ing2 := New(st2)
	r, err := ing2.Apply(ctx, node2, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 4000, Tx: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if r.Delta.Reason != ReasonNormal {
		t.Errorf("面板重启不该被当成节点重启，实际 %q", r.Delta.Reason)
	}
	if r.Usage.RxBytes != 3000 {
		t.Errorf("面板重启后累计 rx = %d，期望 3000", r.Usage.RxBytes)
	}
}

func TestApplyWritesHourlyBuckets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	node := newTestNode(t, st)

	ing := New(st)
	fixed := time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)
	ing.now = func() time.Time { return fixed }

	ing.Apply(ctx, node, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 100, Tx: 100})
	ing.Apply(ctx, node, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 600, Tx: 900})

	pts, err := st.HourlyRange(ctx, node.ID, fixed.Add(-2*time.Hour), fixed.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("期望 1 个小时桶，实际 %d 个", len(pts))
	}
	if !pts[0].HourTS.Equal(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("小时桶时间戳 = %s，期望 10:00", pts[0].HourTS)
	}
	if pts[0].RxDelta != 500 || pts[0].TxDelta != 800 {
		t.Errorf("小时桶 = %d/%d，期望 500/800", pts[0].RxDelta, pts[0].TxDelta)
	}
}

func TestApplyUpdatesHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	node := newTestNode(t, st)
	ing := New(st)

	if _, err := ing.Apply(ctx, node, protocol.Report{BootID: "b1", Iface: "eth0", Rx: 1, Tx: 1}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusOnline {
		t.Errorf("上报后状态 = %q，期望 online", got.Status)
	}
	if got.LastSeen == nil {
		t.Error("上报后 last_seen 不该为空")
	}
}

// 面板自己设的那几个状态，探针的后续心跳都不该把它们刷回 online。
//
// 这条路径就是**线上真正走的那条** —— 入账和心跳在同一个事务里，用的是
// Tx.TouchNode。它和 Store.TouchNode 曾经各写了一遍 SQL，加 planned_stop
// 时只改了非事务那份，于是关机窗口里每一次上报都把节点刷成 online，
// 等机器真断电就误报一条掉线。用户报的「关联了 CDT 定时关机还是提示离线」
// 就是这么来的。
//
// 表驱动而不是只测 exceeded：store 那边再加状态时，这里漏掉就会挂。
func TestTouchDoesNotOverridePanelOwnedStatus(t *testing.T) {
	ctx := context.Background()

	// 与 store.panelOwnedStatuses 对齐。
	for _, status := range []string{
		store.StatusExceeded,
		store.StatusStopped,
		store.StatusPlannedStop,
	} {
		st := newTestStore(t)
		node := newTestNode(t, st)
		ing := New(st)

		if err := st.SetNodeStatus(ctx, node.ID, status); err != nil {
			t.Fatal(err)
		}
		// 机器还没真关掉，探针还会再报几次。
		if _, err := ing.Apply(ctx, node,
			protocol.Report{BootID: "b1", Iface: "eth0", Rx: 1, Tx: 1}); err != nil {
			t.Fatal(err)
		}

		got, _ := st.GetNode(ctx, node.ID)
		if got.Status != status {
			t.Errorf("状态 %q 被上报覆盖成了 %q", status, got.Status)
		}
	}
}
