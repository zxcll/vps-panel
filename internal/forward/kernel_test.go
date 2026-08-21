package forward

import (
	"io"
	"log/slog"
	"testing"
)

// 这组用例钉的是一个真实踩过的坑：EnableIPForward 曾经写好了却没有任何调用点，
// 于是 nft 规则一切正常、conntrack 里空空如也、抓包只看得到 SYN 进来。
// 只测「有没有去开」，不测真的 sysctl —— 那要 root。
func withFakeSysctl(t *testing.T, v4, v6 bool) (enabled *int) {
	t.Helper()
	oldGet4, oldGet6, oldSet := ipForwardEnabled, ipv6ForwardEnabled, enableIPForward
	t.Cleanup(func() { ipForwardEnabled, ipv6ForwardEnabled, enableIPForward = oldGet4, oldGet6, oldSet })

	calls := 0
	cur4, cur6 := v4, v6
	ipForwardEnabled = func() bool { return cur4 }
	ipv6ForwardEnabled = func() bool { return cur6 }
	enableIPForward = func() error { calls++; cur4, cur6 = true, true; return nil }
	return &calls
}

func newTestKernel(t *testing.T) *kernelBackend {
	t.Helper()
	return &kernelBackend{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestReconcileTurnsOnIPForward(t *testing.T) {
	calls := withFakeSysctl(t, false, false)
	k := newTestKernel(t)

	k.ensureIPForward([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
	})
	if *calls != 1 {
		t.Fatalf("有内核规则且开关是关的，应当去开一次，实际调用 %d 次", *calls)
	}
}

func TestIPForwardNotTouchedWhenAlreadyOn(t *testing.T) {
	calls := withFakeSysctl(t, true, true)
	k := newTestKernel(t)
	k.ensureIPForward([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
	})
	if *calls != 0 {
		t.Errorf("开关已经是开的就不该再动系统设置，实际调用 %d 次", *calls)
	}
}

func TestIPForwardNotTouchedWithoutKernelRules(t *testing.T) {
	// 没配转发的机器不该被我们改内核参数。探针默认开着转发功能，
	// 绝大多数机器上一条规则都不会有。
	calls := withFakeSysctl(t, false, false)
	k := newTestKernel(t)
	k.ensureIPForward(nil)
	if *calls != 0 {
		t.Errorf("没有内核规则时不该动系统设置，实际调用 %d 次", *calls)
	}
}

func TestIPv6TargetTurnsOnIPv6Forward(t *testing.T) {
	// IPv4 开着但 IPv6 没开，而规则的目标是 IPv6 —— 仍然要去开一次。
	calls := withFakeSysctl(t, true, false)
	k := newTestKernel(t)
	k.ensureIPForward([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2001:db8::1", DestPort: 443},
	})
	if *calls != 1 {
		t.Errorf("目标是 IPv6 且 IPv6 转发没开，应当去开一次，实际调用 %d 次", *calls)
	}
}
