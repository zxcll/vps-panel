package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newForwardTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
}

func newForwardTestNode(t *testing.T, st *Store, name string) *Node {
	t.Helper()
	n := &Node{
		Name: name, Secret: "secret-" + t.Name() + "-" + name,
		BillingMode: BillingSum, TrafficRatio: 1, WarnPercent: 90,
		ResetDay: 1, ResetTZ: "UTC", Status: StatusUnknown, Enabled: true, SSHPort: 22,
	}
	if err := st.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	return n
}

// 迁移是每次启动全量重放的，没有版本表。这条用例钉住"重放必须幂等"——
// 不幂等的话面板第二次启动就直接起不来了。
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st1 := openTestStore(t, path)
	st1.Close()

	// 第二次、第三次打开同一个库，迁移会再跑一遍。
	openTestStore(t, path).Close()
	st3 := openTestStore(t, path)

	// 表还在、还能用。
	if _, err := st3.ListForwardRules(context.Background()); err != nil {
		t.Fatalf("重放迁移后转发表不可用: %v", err)
	}
}

func TestForwardRuleCRUD(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	a := newForwardTestNode(t, st, "中转A")
	b := newForwardTestNode(t, st, "落地B")

	r := &ForwardRule{
		Name: "香港中转", Proto: ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []ForwardHop{
			{NodeID: a.ID, ListenPort: 8443, Mode: ForwardModeKernel},
			{NodeID: b.ID, ListenPort: 9000, Mode: ForwardModeUserspace, BandwidthMbps: 50},
		},
	}
	if err := st.CreateForwardRule(ctx, r); err != nil {
		t.Fatalf("建规则失败: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("建完应回填规则 ID")
	}
	for i, h := range r.Hops {
		if h.ID == 0 {
			t.Errorf("第 %d 跳应回填 ID", i)
		}
		if h.Position != i {
			t.Errorf("第 %d 跳的 position = %d，应按切片顺序自动编号", i, h.Position)
		}
		if h.Proto != ForwardProtoTCP {
			t.Errorf("第 %d 跳的 proto 应从规则冗余下来，实际 %q", i, h.Proto)
		}
	}

	got, err := st.GetForwardRule(ctx, r.ID)
	if err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	if len(got.Hops) != 2 || got.Hops[0].NodeID != a.ID || got.Hops[1].NodeID != b.ID {
		t.Fatalf("跳的顺序不对：%+v", got.Hops)
	}
	if got.Hops[0].NodeName != "中转A" {
		t.Errorf("应带出节点名，实际 %q", got.Hops[0].NodeName)
	}

	// 改成单跳。
	got.Name = "改名了"
	got.Hops = got.Hops[:1]
	if err := st.UpdateForwardRule(ctx, got); err != nil {
		t.Fatalf("改规则失败: %v", err)
	}
	after, _ := st.GetForwardRule(ctx, r.ID)
	if after.Name != "改名了" || len(after.Hops) != 1 {
		t.Fatalf("更新没生效：%+v", after)
	}

	if err := st.DeleteForwardRule(ctx, r.ID); err != nil {
		t.Fatalf("删规则失败: %v", err)
	}
	if _, err := st.GetForwardRule(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删掉后应返回 ErrNotFound，实际 %v", err)
	}
	// 跳应该被级联删掉。
	hops, _ := st.ForwardHopsOnNode(ctx, a.ID)
	if len(hops) != 0 {
		t.Errorf("删规则后跳应级联清掉，实际还剩 %d 条", len(hops))
	}
}

func TestForwardHopPortUniquePerProto(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	a := newForwardTestNode(t, st, "中转A")

	mk := func(proto string, port int) error {
		return st.CreateForwardRule(ctx, &ForwardRule{
			Name: proto, Proto: proto, DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
			Hops: []ForwardHop{{NodeID: a.ID, ListenPort: port, Mode: ForwardModeKernel}},
		})
	}

	if err := mk(ForwardProtoTCP, 8443); err != nil {
		t.Fatalf("第一条应当成功: %v", err)
	}
	// 同端口不同协议是两个坑位，应当允许。
	if err := mk(ForwardProtoUDP, 8443); err != nil {
		t.Fatalf("同端口不同协议应当允许: %v", err)
	}
	// 同端口同协议必须被数据库挡住。
	if err := mk(ForwardProtoTCP, 8443); err == nil {
		t.Error("同节点同端口同协议应当被唯一约束拒绝")
	}
}

func TestForwardNodeDefaults(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	n := newForwardTestNode(t, st, "节点")

	// 没配过的节点要返回一份可用的默认值，而不是报错。
	fn, err := st.GetForwardNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("读转发配置失败: %v", err)
	}
	if fn.PortStart != DefaultForwardPortStart || fn.PortEnd != DefaultForwardPortEnd || !fn.Enabled {
		t.Errorf("默认配置不对：%+v", fn)
	}

	fn.RelayHost = "203.0.113.7"
	fn.PortStart, fn.PortEnd = 30000, 30100
	if err := st.SaveForwardNode(ctx, fn); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 再存一次走 UPSERT 分支。
	if err := st.SaveForwardNode(ctx, fn); err != nil {
		t.Fatalf("重复保存失败: %v", err)
	}
	got, _ := st.GetForwardNode(ctx, n.ID)
	if got.RelayHost != "203.0.113.7" || got.PortStart != 30000 {
		t.Errorf("保存后读回不对：%+v", got)
	}
}

func TestForwardLedgerIsSeparateFromNodeLedger(t *testing.T) {
	// 这条钉住本方案最核心的一条铁律：转发账本一个字节都不能进节点账本。
	st := newForwardTestStore(t)
	ctx := context.Background()
	n := newForwardTestNode(t, st, "节点")

	r := &ForwardRule{
		Name: "规则", Proto: ForwardProtoTCP, DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []ForwardHop{{NodeID: n.ID, ListenPort: 8443, Mode: ForwardModeKernel}},
	}
	if err := st.CreateForwardRule(ctx, r); err != nil {
		t.Fatalf("建规则失败: %v", err)
	}
	hopID := r.Hops[0].ID
	now := time.Now().UTC()

	err := st.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.AddForwardUsage(ctx, hopID, 1000, 2000, now, now)
	})
	if err != nil {
		t.Fatalf("写转发用量失败: %v", err)
	}

	usage, err := st.GetUsage(ctx, n.ID)
	if err != nil {
		t.Fatalf("读节点账本失败: %v", err)
	}
	if usage.RxBytes != 0 || usage.TxBytes != 0 {
		t.Fatalf("转发用量污染了节点账本：rx=%d tx=%d", usage.RxBytes, usage.TxBytes)
	}

	fwd, _ := st.AllForwardUsage(ctx)
	if fwd[hopID] == nil || fwd[hopID].UpBytes != 1000 || fwd[hopID].DownBytes != 2000 {
		t.Errorf("转发账本记错了：%+v", fwd[hopID])
	}
}

func TestForwardCounterEmptyEpochMeansNoBaseline(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	n := newForwardTestNode(t, st, "节点")
	r := &ForwardRule{
		Name: "规则", Proto: ForwardProtoTCP, DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []ForwardHop{{NodeID: n.ID, ListenPort: 8443, Mode: ForwardModeKernel}},
	}
	if err := st.CreateForwardRule(ctx, r); err != nil {
		t.Fatalf("建规则失败: %v", err)
	}
	hopID := r.Hops[0].ID

	err := st.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		// 还没上报过：应当返回 nil 而不是一条零值记录，
		// 否则首次上报会被当成"从 0 涨到 N"，把探针装上之前的历史流量凭空计入。
		prev, err := tx.GetForwardCounter(ctx, hopID)
		if err != nil {
			return err
		}
		if prev != nil {
			t.Errorf("还没上报过时应返回 nil，实际 %+v", prev)
		}

		if err := tx.SaveForwardCounter(ctx, &ForwardCounter{
			HopID: hopID, Epoch: "e1", LastUp: 500, LastDown: 600, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		got, err := tx.GetForwardCounter(ctx, hopID)
		if err != nil {
			return err
		}
		if got == nil || got.Epoch != "e1" || got.LastUp != 500 {
			t.Errorf("保存后读回不对：%+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

func TestHopExistsGuardsAgainstDeletedHops(t *testing.T) {
	// 探针会继续上报刚被删掉的跳（要把最后一段流量送回来），
	// 面板必须认得出来并跳过，否则外键会让整个上报事务回滚。
	st := newForwardTestStore(t)
	ctx := context.Background()

	err := st.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		ok, err := tx.HopExists(ctx, 99999)
		if err != nil {
			return err
		}
		if ok {
			t.Error("不存在的 hop_id 应返回 false")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}
