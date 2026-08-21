package forward

import (
	"strings"
	"testing"
)

// mustContain 断言渲染结果里有这一行（忽略缩进）。
func mustContain(t *testing.T, script, want string) {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Errorf("规则集里缺少这一行：\n  %s\n实际渲染结果：\n%s", want, script)
}

func mustNotContain(t *testing.T, script, unwanted string) {
	t.Helper()
	if strings.Contains(script, unwanted) {
		t.Errorf("规则集里不该出现 %q，实际渲染结果：\n%s", unwanted, script)
	}
}

func TestRenderIPv4DNAT(t *testing.T) {
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
	})

	mustContain(t, s, "tcp dport 8443 dnat ip to 1.2.3.4:443")
	mustContain(t, s, "ip daddr 1.2.3.4 tcp dport 443 ct status dnat masquerade")
	// 计数必须按 conntrack 的 original 端口来匹配：DNAT 之后包上的目标端口
	// 已经是 443 了，只有 conntrack 还记得用户填的 8443。
	mustContain(t, s, "meta l4proto tcp ct original proto-dst 8443 ct direction original counter")
	mustContain(t, s, "meta l4proto tcp ct original proto-dst 8443 ct direction reply counter")
	// 没有限速规则时不该渲染 restore_mark 链。
	mustNotContain(t, s, "restore_mark")
	// 没有回环目标时不该渲染 input/output 计数链。
	mustNotContain(t, s, "account_local")
}

func TestRenderIPv6UsesIP6Syntax(t *testing.T) {
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2001:db8::1", DestPort: 443},
	})
	mustContain(t, s, "tcp dport 8443 dnat ip6 to [2001:db8::1]:443")
	mustContain(t, s, "ip6 daddr 2001:db8::1 tcp dport 443 ct status dnat masquerade")
}

func TestRenderTCPUDPUsesL4ProtoSet(t *testing.T) {
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoBoth, ListenPort: 7000, DestIP: "5.6.7.8", DestPort: 7000},
	})
	mustContain(t, s, "meta l4proto { tcp, udp } th dport 7000 dnat ip to 5.6.7.8:7000")
	// 计数链必须按具体协议分开写：nftables 没法在 l4proto 集合下给
	// ct proto-dst 定类型，合在一起写出来的计数是读不回来的。
	mustContain(t, s, "meta l4proto tcp ct original proto-dst 7000 ct direction original counter")
	mustContain(t, s, "meta l4proto udp ct original proto-dst 7000 ct direction original counter")
	mustNotContain(t, s, "meta l4proto { tcp, udp } ct original")
}

func TestRenderIPv4LoopbackUsesInputOutputChains(t *testing.T) {
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8080, DestIP: "127.0.0.1", DestPort: 80},
	})
	mustContain(t, s, "tcp dport 8080 dnat ip to 127.0.0.1:80")
	// 回环目标不离开本机，masquerade 是多余的。
	mustNotContain(t, s, "masquerade")
	// 也不走 forward 链，所以要挂在 input/output 上计数。
	mustContain(t, s, "meta l4proto tcp ct original proto-dst 8080 ct status dnat ct direction original counter")
	mustContain(t, s, "type filter hook input priority filter; policy accept;")
	mustContain(t, s, "type filter hook output priority filter; policy accept;")
}

func TestRenderIPv6LoopbackUsesRedirect(t *testing.T) {
	// IPv6 没有 route_localnet 等价物，DNAT 到 ::1 路由不过去，只能用 redirect。
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8080, DestIP: "::1", DestPort: 80},
	})
	mustContain(t, s, "meta nfproto ipv6 tcp dport 8080 redirect to :80")
	mustNotContain(t, s, "dnat ip6 to [::1]")
}

func TestRenderShapedRuleEmitsMarkAndRestoreChain(t *testing.T) {
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443, BandwidthMbps: 50},
	})
	// mark 要同时打在包和 conntrack 上：nat prerouting 只看得到第一个包。
	mustContain(t, s, "tcp dport 8443 meta mark set 0x120fb ct mark set meta mark dnat ip to 1.2.3.4:443")
	// 恢复链必须带掩码，否则会把别人设的 ct mark 也抢过来。
	mustContain(t, s, "ct mark and 0xffff0000 == 0x10000 meta mark set ct mark")
}

func TestRenderSkipsUnresolvedRule(t *testing.T) {
	// 域名还没解析出来的规则要整条跳过，不能渲染出半截语句把整张表 apply 失败。
	s := RenderRuleset([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestHost: "hk.example.com", DestPort: 443},
		{HopID: 2, Proto: ProtoTCP, ListenPort: 8444, DestIP: "1.2.3.4", DestPort: 443},
	})
	mustNotContain(t, s, "8443")
	mustContain(t, s, "tcp dport 8444 dnat ip to 1.2.3.4:443")
}

func TestRenderEmptyRulesetIsStillValidTable(t *testing.T) {
	s := RenderRuleset(nil)
	mustContain(t, s, "table inet vps_forward {")
	mustContain(t, s, "type nat hook prerouting priority dstnat; policy accept;")
	if !strings.HasSuffix(s, "}\n") {
		t.Errorf("表定义没有正常闭合：\n%s", s)
	}
}

func TestShapeMarkNamespacing(t *testing.T) {
	// mark 的高 16 位固定是 0x0001，这样 restore_mark 的掩码才能把
	// 我们的 mark 和别的组件（策略路由、其他 QoS）区分开。
	r := Rule{ListenPort: 8443, BandwidthMbps: 10}
	if got := r.ShapeMark(); got != 0x120fb {
		t.Errorf("ShapeMark = 0x%x，期望 0x120fb", got)
	}
	if got := (Rule{ListenPort: 8443}).ShapeMark(); got != 0 {
		t.Errorf("不限速的规则 ShapeMark 应为 0，实际 0x%x", got)
	}
}
