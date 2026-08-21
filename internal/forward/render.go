package forward

import (
	"fmt"
	"strings"
)

// RenderRuleset 把一组**内核态**规则渲染成 nft 脚本片段（只含 table 定义本身，
// 不含建表/删表语句，那部分在 Apply 里拼）。
//
// 目标 IP 为空的规则会被跳过：那是域名还没解析出来，宁可这条不通，
// 也不能渲染出一条语法错误的规则把整张表都 apply 失败。
//
// 表里一共可能出现 5 条链：
//
//	prerouting          nat/dstnat    —— DNAT 到目标，顺带给限速规则打 mark
//	restore_mark        filter/mangle —— 把 ct mark 恢复到 meta mark（只在有限速规则时出现）
//	postrouting         nat/srcnat    —— masquerade，让回程能找回来
//	account             filter/forward—— 每条规则两个方向各一个 counter
//	account_local[_reply] filter/in+out—— 回环目标专用，它们不走 forward 链
func RenderRuleset(rules []Rule) string {
	var b strings.Builder

	hasLoopback := false
	hasShaped := false
	for _, r := range rules {
		if r.DestIP == "" {
			continue
		}
		if IsLoopback(r.DestIP) {
			hasLoopback = true
		}
		if r.ShapeMark() != 0 {
			hasShaped = true
		}
	}

	b.WriteString(fmt.Sprintf("table %s %s {\n", TableFamily, TableName))

	// ---- prerouting：DNAT ----
	b.WriteString("\tchain prerouting {\n")
	b.WriteString("\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, r := range rules {
		if r.DestIP == "" {
			continue
		}
		mark := ""
		if m := r.ShapeMark(); m != 0 {
			// 一句话同时给包和它的 conntrack 条目打上 mark。
			// nat prerouting 只看得到一条连接的第一个包，所以必须借 ct mark 存下来，
			// 后续包（含回程）由 restore_mark 链重新贴上。
			mark = fmt.Sprintf("meta mark set 0x%x ct mark set meta mark ", m)
		}
		if IsLoopback(r.DestIP) && IsIPv6(r.DestIP) {
			// IPv6 没有 route_localnet 这种开关，DNAT 到 ::1 路由不过去。
			// redirect 直接把包交给本机，绕开这个问题。
			b.WriteString(fmt.Sprintf("\t\tmeta nfproto ipv6 %s %sredirect to :%d\n",
				protoDportMatch(r.Proto, r.ListenPort), mark, r.DestPort))
			continue
		}
		b.WriteString(fmt.Sprintf("\t\t%s %s%s\n",
			protoDportMatch(r.Proto, r.ListenPort), mark, dnatTarget(r.DestIP, r.DestPort)))
	}
	b.WriteString("\t}\n")

	// ---- restore_mark：把 ct mark 贴回每个包 ----
	if hasShaped {
		b.WriteString("\tchain restore_mark {\n")
		b.WriteString("\t\ttype filter hook prerouting priority mangle; policy accept;\n")
		// 只恢复高 16 位是 0x0001 的 mark（见 Rule.ShapeMark 的偏移约定）。
		// 写成无条件的 "ct mark != 0" 会把别人（策略路由、其他 QoS）设的 ct mark
		// 也抢过来，把它们的包拽进我们的 tc 分类里。
		b.WriteString("\t\tct mark and 0xffff0000 == 0x10000 meta mark set ct mark\n")
		b.WriteString("\t}\n")
	}

	// ---- postrouting：masquerade ----
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, r := range rules {
		// 回环目标不需要 masquerade —— 包根本没离开本机。
		if r.DestIP == "" || IsLoopback(r.DestIP) {
			continue
		}
		b.WriteString(fmt.Sprintf("\t\t%s %s ct status dnat masquerade\n",
			daddrMatch(r.DestIP), protoDportMatch(r.Proto, r.DestPort)))
	}
	b.WriteString("\t}\n")

	// ---- account：转发流量计数 ----
	b.WriteString("\tchain account {\n")
	b.WriteString("\t\ttype filter hook forward priority filter; policy accept;\n")
	// 每条规则两个方向各写一条 counter，是刻意的：上行和下行要分开展示。
	// 不要合并成一个方向无关的 counter，那样就丢掉了 up/down 拆分。
	//
	// 用 "ct original proto-dst" 而不是直接匹配 dport：DNAT 之后包的目标端口
	// 已经变成 DestPort 了，只有 conntrack 里的 original 元组还留着用户看得懂的监听端口。
	for _, r := range rules {
		if r.DestIP == "" || IsLoopback(r.DestIP) {
			continue
		}
		for _, p := range splitProtos(r.Proto) {
			b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction original counter\n", p, r.ListenPort))
			b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction reply counter\n", p, r.ListenPort))
		}
	}
	b.WriteString("\t}\n")

	// ---- account_local：回环目标的计数 ----
	// 目标是本机时包不走 forward 链，得在 input/output 上分别计数。
	if hasLoopback {
		b.WriteString("\tchain account_local {\n")
		b.WriteString("\t\ttype filter hook input priority filter; policy accept;\n")
		for _, r := range rules {
			if r.DestIP == "" || !IsLoopback(r.DestIP) {
				continue
			}
			for _, p := range splitProtos(r.Proto) {
				b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction original counter\n", p, r.ListenPort))
			}
		}
		b.WriteString("\t}\n")
		b.WriteString("\tchain account_local_reply {\n")
		b.WriteString("\t\ttype filter hook output priority filter; policy accept;\n")
		for _, r := range rules {
			if r.DestIP == "" || !IsLoopback(r.DestIP) {
				continue
			}
			for _, p := range splitProtos(r.Proto) {
				b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction reply counter\n", p, r.ListenPort))
			}
		}
		b.WriteString("\t}\n")
	}

	b.WriteString("}\n")
	return b.String()
}

// protoDportMatch 拼出「协议 + 目标端口」的匹配子句。
// tcp+udp 要用 l4proto 集合语法，nft 才认这种多协议匹配。
func protoDportMatch(proto string, port int) string {
	if proto == ProtoBoth {
		return fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", port)
	}
	return fmt.Sprintf("%s dport %d", proto, port)
}

// dnatTarget 拼 inet 表里的 DNAT 目标。
// IPv4 写 "dnat ip to 1.2.3.4:443"，IPv6 写 "dnat ip6 to [::1]:443"。
func dnatTarget(ip string, port int) string {
	if IsIPv6(ip) {
		return fmt.Sprintf("dnat ip6 to [%s]:%d", ip, port)
	}
	return fmt.Sprintf("dnat ip to %s:%d", ip, port)
}

// daddrMatch 按 IP 族拼目标地址匹配。
func daddrMatch(ip string) string {
	if IsIPv6(ip) {
		return "ip6 daddr " + ip
	}
	return "ip daddr " + ip
}
