package forward

import "fmt"

// Partition 把规则集拆成内核态和用户态两份。
//
// 一条 tcp+udp 的用户态规则会被拆成两条：UDP 那半走内核态（用户态不支持 UDP），
// TCP 那半走用户态，目标和限速都跟着复制一份。
//
// 同时做端口占用检查：把 tcp+udp 视为同时占了 tcp/端口 和 udp/端口。
// 这一层能顺带抓到「一条 tcp+udp 和一条 tcp 撞同一个端口」这种情况 ——
// 上层按协议字符串去重是看不出来的，因为 "tcp+udp" != "tcp"。
func Partition(rules []Rule) (kernel, userspace []Rule, err error) {
	claimed := map[string]string{} // "tcp/8443" -> 占用者描述

	claim := func(proto string, port int, who string) error {
		for _, p := range splitProtos(proto) {
			key := fmt.Sprintf("%s/%d", p, port)
			if prev, dup := claimed[key]; dup {
				return fmt.Errorf("端口 %s 被重复占用：%s 和 %s", key, prev, who)
			}
			claimed[key] = who
		}
		return nil
	}

	for _, r := range rules {
		who := fmt.Sprintf("规则 #%d(%s/%d, %s)", r.HopID, r.Proto, r.ListenPort, r.EffectiveMode())

		if r.EffectiveMode() == ModeKernel {
			if cerr := claim(r.Proto, r.ListenPort, who); cerr != nil {
				return nil, nil, cerr
			}
			kernel = append(kernel, r)
			continue
		}

		switch r.Proto {
		case ProtoTCP:
			if cerr := claim(ProtoTCP, r.ListenPort, who); cerr != nil {
				return nil, nil, cerr
			}
			userspace = append(userspace, r)
		case ProtoBoth:
			if cerr := claim(ProtoBoth, r.ListenPort, who); cerr != nil {
				return nil, nil, cerr
			}
			udp := r
			udp.Proto = ProtoUDP
			udp.Mode = ModeKernel
			kernel = append(kernel, udp)

			tcp := r
			tcp.Proto = ProtoTCP
			tcp.Mode = ModeUserspace
			userspace = append(userspace, tcp)
		default:
			// Validate 已经拦过 udp+userspace，这里是防御性兜底。
			return nil, nil, fmt.Errorf("规则 #%d: 协议 %s 不支持用户态转发", r.HopID, r.Proto)
		}
	}
	return kernel, userspace, nil
}
