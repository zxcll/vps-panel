package forward

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeKernel 顶替真正的内核后端 —— 真的那个要 root 才能跑。
// 它模拟了最关键的一个行为：每次 Reconcile 都把计数清零（nft 是删表重建）。
type fakeKernel struct {
	rules    []Rule
	counters map[int64][2]int64 // hopID -> [上行, 下行]
	failNext error
	applies  int
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{counters: map[int64][2]int64{}}
}

func (f *fakeKernel) Reconcile(rules []Rule) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	f.applies++
	f.rules = rules
	// nft.Apply 是 delete table + 重建，计数全归零。这正是要测的那件事。
	f.counters = map[int64][2]int64{}
	return nil
}

func (f *fakeKernel) Counters() ([]Counter, error) {
	out := make([]Counter, 0, len(f.counters))
	for hopID, v := range f.counters {
		out = append(out, Counter{HopID: hopID, BytesUp: v[0], BytesDown: v[1]})
	}
	return out, nil
}

func (f *fakeKernel) Close() {}

// traffic 模拟一段流量跑过内核。
func (f *fakeKernel) traffic(hopID, up, down int64) {
	v := f.counters[hopID]
	f.counters[hopID] = [2]int64{v[0] + up, v[1] + down}
}

// newTestDataplane 造一个只有假内核后端的数据面。
// 用户态后端是真的，但测试里不给它任何用户态规则，所以不会真去绑端口。
func newTestDataplane(k kernelReconciler) *Dataplane {
	return &Dataplane{
		kernel:      k,
		userspace:   newUserspaceBackend(0),
		fw:          &firewallSet{}, // 空集合：不去碰宿主机的防火墙
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:         func() time.Time { return time.Now() },
		logical:     map[int64][2]int64{},
		lastRaw:     map[int64][2]int64{},
		absentSince: map[int64]time.Time{},
	}
}

func counterOf(t *testing.T, dp *Dataplane, hopID int64) Counter {
	t.Helper()
	for _, c := range dp.Counters() {
		if c.HopID == hopID {
			return c
		}
	}
	t.Fatalf("没有 hop %d 的计数", hopID)
	return Counter{}
}

// 这是 1.4 节设计的验收点：改规则会把 nft 计数清零，但对外的累计量必须连续。
func TestCountersStayMonotonicAcrossApply(t *testing.T) {
	k := newFakeKernel()
	dp := newTestDataplane(k)
	rules := []Rule{{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443}}

	if err := dp.Reconcile(rules); err != nil {
		t.Fatalf("首次下发失败: %v", err)
	}

	k.traffic(1, 1000, 2000)
	if c := counterOf(t, dp, 1); c.BytesUp != 1000 || c.BytesDown != 2000 {
		t.Fatalf("第一段流量后 = %d/%d，期望 1000/2000", c.BytesUp, c.BytesDown)
	}

	k.traffic(1, 500, 300)
	if c := counterOf(t, dp, 1); c.BytesUp != 1500 || c.BytesDown != 2300 {
		t.Fatalf("第二段流量后 = %d/%d，期望 1500/2300", c.BytesUp, c.BytesDown)
	}

	// 又跑了一段还没被采样，然后用户改了规则触发 apply。
	// Reconcile 必须先采样把这一段结转进来，再让内核清零。
	k.traffic(1, 700, 100)
	rules[0].DestPort = 8443
	if err := dp.Reconcile(rules); err != nil {
		t.Fatalf("重新下发失败: %v", err)
	}
	if c := counterOf(t, dp, 1); c.BytesUp != 2200 || c.BytesDown != 2400 {
		t.Fatalf("apply 前的最后一段流量丢了：= %d/%d，期望 2200/2400", c.BytesUp, c.BytesDown)
	}

	// 清零之后新跑的流量要接着往上加，不能从头开始。
	k.traffic(1, 100, 200)
	if c := counterOf(t, dp, 1); c.BytesUp != 2300 || c.BytesDown != 2600 {
		t.Fatalf("清零后续算错误：= %d/%d，期望 2300/2600", c.BytesUp, c.BytesDown)
	}
}

func TestCounterStateSurvivesRestart(t *testing.T) {
	k := newFakeKernel()
	dp := newTestDataplane(k)
	rules := []Rule{{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443}}
	if err := dp.Reconcile(rules); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	k.traffic(1, 5000, 6000)
	_ = dp.Counters()
	saved := dp.CounterState()

	// 探针重启：进程没了，但内核里的 nftables 表还在，计数继续涨。
	k.traffic(1, 1000, 2000)

	dp2 := newTestDataplane(k)
	dp2.SeedCounters(saved)
	// 重启后第一次 Reconcile 会先采样 —— 因为 LastRaw 也存了下来，
	// 探针不在的那段时间（内核照样在转发）的流量能靠差值补回来。
	if err := dp2.Reconcile(rules); err != nil {
		t.Fatalf("重启后下发失败: %v", err)
	}
	if c := counterOf(t, dp2, 1); c.BytesUp != 6000 || c.BytesDown != 8000 {
		t.Fatalf("重启后累计 = %d/%d，期望 6000/8000（含探针不在时的 1000/2000）", c.BytesUp, c.BytesDown)
	}
}

func TestCountersNotDoubleCountedOnRestartWithoutTraffic(t *testing.T) {
	// 上一个用例的反面：探针重启期间一个字节都没跑，重启后不能凭空多算。
	k := newFakeKernel()
	dp := newTestDataplane(k)
	rules := []Rule{{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443}}
	_ = dp.Reconcile(rules)
	k.traffic(1, 5000, 6000)
	_ = dp.Counters()
	saved := dp.CounterState()

	dp2 := newTestDataplane(k)
	dp2.SeedCounters(saved)
	_ = dp2.Reconcile(rules)
	if c := counterOf(t, dp2, 1); c.BytesUp != 5000 || c.BytesDown != 6000 {
		t.Fatalf("重启后重复计数了：= %d/%d，期望 5000/6000", c.BytesUp, c.BytesDown)
	}
}

func TestFoldDelta(t *testing.T) {
	cases := []struct {
		name      string
		last, cur int64
		want      int64
	}{
		{"正常递增", 1000, 1500, 500},
		{"没动过", 1000, 1000, 0},
		{"计数器清零后重新累积", 1000, 300, 300},
		{"计数器刚清零还没跑流量", 1000, 0, 0},
		{"首次采样", 0, 800, 800},
		{"负数当 0 处理", 1000, -5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldDelta(tc.last, tc.cur); got != tc.want {
				t.Errorf("foldDelta(%d, %d) = %d，期望 %d", tc.last, tc.cur, got, tc.want)
			}
		})
	}
}

func TestReconcileRejectsConflictingRules(t *testing.T) {
	k := newFakeKernel()
	dp := newTestDataplane(k)
	err := dp.Reconcile([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
		{HopID: 2, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2.2.2.2", DestPort: 2},
	})
	if err == nil {
		t.Fatal("端口冲突应该在下发前就被拦下")
	}
	if k.applies != 0 {
		t.Errorf("规则集不合法时不该碰内核，实际 apply 了 %d 次", k.applies)
	}
}

func TestReconcileKernelFailureIsReported(t *testing.T) {
	k := newFakeKernel()
	dp := newTestDataplane(k)
	rules := []Rule{{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443}}

	boom := errors.New("nft 挂了")
	k.failNext = boom
	if err := dp.Reconcile(rules); !errors.Is(err, boom) {
		t.Fatalf("内核失败应原样往上抛，实际 %v", err)
	}
	// 失败之后还能正常重试。
	if err := dp.Reconcile(rules); err != nil {
		t.Fatalf("重试应当成功，实际 %v", err)
	}
}

func TestStaleHopCountersArePrunedEventually(t *testing.T) {
	k := newFakeKernel()
	dp := newTestDataplane(k)
	now := time.Unix(1_700_000_000, 0)
	dp.now = func() time.Time { return now }

	twoHops := []Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
		{HopID: 2, Proto: ProtoTCP, ListenPort: 8444, DestIP: "2.2.2.2", DestPort: 2},
	}
	_ = dp.Reconcile(twoHops)
	k.traffic(1, 100, 100)
	k.traffic(2, 200, 200)
	_ = dp.Counters()

	// 删掉 hop 2。它的计数要先留着，等最后一段流量被上报出去。
	_ = dp.Reconcile(twoHops[:1])
	if len(dp.Counters()) != 2 {
		t.Fatalf("刚删掉的跳应保留计数等待上报，实际 %d 条", len(dp.Counters()))
	}

	// 过了保留期才真正清掉，免得长期运行的探针无限攒 map。
	now = now.Add(stalePruneAfter + time.Minute)
	_ = dp.Reconcile(twoHops[:1])
	got := dp.Counters()
	if len(got) != 1 || got[0].HopID != 1 {
		t.Fatalf("过期后应只剩 hop 1，实际 %+v", got)
	}
}
