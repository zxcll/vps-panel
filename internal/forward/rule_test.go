package forward

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Rule{Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443}

	cases := []struct {
		name    string
		mutate  func(r *Rule)
		wantErr string // 空串表示应当通过
	}{
		{"最小合法规则", func(*Rule) {}, ""},
		{"域名目标", func(r *Rule) { r.DestIP = ""; r.DestHost = "hk.example.com" }, ""},
		{"域名加已解析 IP", func(r *Rule) { r.DestHost = "hk.example.com" }, ""},
		{"用户态 TCP", func(r *Rule) { r.Mode = ModeUserspace }, ""},
		{"用户态 tcp+udp", func(r *Rule) { r.Proto = ProtoBoth; r.Mode = ModeUserspace }, ""},
		{"限速", func(r *Rule) { r.BandwidthMbps = 50 }, ""},

		{"协议非法", func(r *Rule) { r.Proto = "sctp" }, "协议"},
		{"监听端口为 0", func(r *Rule) { r.ListenPort = 0 }, "监听端口"},
		{"监听端口越界", func(r *Rule) { r.ListenPort = 70000 }, "监听端口"},
		{"目标端口为 0", func(r *Rule) { r.DestPort = 0 }, "目标端口"},
		{"目标为空", func(r *Rule) { r.DestIP = "" }, "必须填 IP 或域名"},
		{"目标 IP 非法", func(r *Rule) { r.DestIP = "1.2.3.999" }, "格式非法"},
		{"目标域名非法", func(r *Rule) { r.DestIP = ""; r.DestHost = "地址" }, "域名"},
		// 把端口误填成域名是很常见的手滑，数字 TLD 不可能存在，提前拦掉。
		{"把端口当成域名填", func(r *Rule) { r.DestIP = ""; r.DestHost = "4212" }, "域名"},
		{"模式非法", func(r *Rule) { r.Mode = "magic" }, "转发模式"},
		{"UDP 不支持用户态", func(r *Rule) { r.Proto = ProtoUDP; r.Mode = ModeUserspace }, "UDP 不支持用户态"},
		{"限速为负", func(r *Rule) { r.BandwidthMbps = -1 }, "限速"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.mutate(&r)
			err := Validate(r)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应当通过校验，实际报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当报错，含 %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误消息 %q 里应含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestEffectiveModeDefaultsToKernel(t *testing.T) {
	cases := map[string]string{
		"":             ModeKernel,
		ModeKernel:     ModeKernel,
		ModeUserspace:  ModeUserspace,
		"someoldvalue": ModeKernel, // 无法识别的值退回默认，不要让老数据把探针搞崩
	}
	for in, want := range cases {
		if got := (Rule{Mode: in}).EffectiveMode(); got != want {
			t.Errorf("Mode %q 的 EffectiveMode = %q，期望 %q", in, got, want)
		}
	}
}

func TestTargetEmptyWhenUnresolved(t *testing.T) {
	if got := (Rule{DestHost: "a.example.com", DestPort: 443}).Target(); got != "" {
		t.Errorf("域名未解析时 Target 应为空串，实际 %q", got)
	}
	if got := (Rule{DestIP: "1.2.3.4", DestPort: 443}).Target(); got != "1.2.3.4:443" {
		t.Errorf("Target = %q，期望 1.2.3.4:443", got)
	}
	if got := (Rule{DestIP: "2001:db8::1", DestPort: 443}).Target(); got != "[2001:db8::1]:443" {
		t.Errorf("IPv6 的 Target 要加方括号，实际 %q", got)
	}
}
