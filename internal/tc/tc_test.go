package tc

import "testing"

func TestPlanClassesIsDeterministic(t *testing.T) {
	// 输入顺序打乱，输出必须一致 —— 生成的脚本要能直接 diff。
	a := planClasses([]Shaped{
		{Mark: 0x120fb, Minor: 8443, BandwidthMbps: 50},
		{Mark: 0x11f90, Minor: 8080, BandwidthMbps: 10},
	})
	b := planClasses([]Shaped{
		{Mark: 0x11f90, Minor: 8080, BandwidthMbps: 10},
		{Mark: 0x120fb, Minor: 8443, BandwidthMbps: 50},
	})
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("应生成 2 个类，实际 %d / %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("第 %d 个类不一致：%+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestPlanClassesSkipsUnshaped(t *testing.T) {
	got := planClasses([]Shaped{
		{Mark: 0, Minor: 8443, BandwidthMbps: 50},      // 没 mark，nft 那边没打标，限不了
		{Mark: 0x11f90, Minor: 8080, BandwidthMbps: 0}, // 不限速
		{Mark: 0x120fb, Minor: 8443, BandwidthMbps: 50},
	})
	if len(got) != 1 {
		t.Fatalf("应只生成 1 个类，实际 %d 个：%+v", len(got), got)
	}
	if got[0].ClassID != "1:20fb" {
		t.Errorf("ClassID = %q，期望 1:20fb（minor 用监听端口的十六进制）", got[0].ClassID)
	}
	if got[0].Handle != "0x120fb" {
		t.Errorf("Handle = %q，期望 0x120fb", got[0].Handle)
	}
	// rate 和 ceil 取同一个值，是硬上限而不是可借用的保证带宽。
	if got[0].Rate != "50mbit" {
		t.Errorf("Rate = %q，期望 50mbit", got[0].Rate)
	}
}

func TestPlanClassesDeduplicatesMinor(t *testing.T) {
	// 一条 tcp+udp 规则在内核那边会拆成两条，minor 相同，只能建一个类
	// （重复 tc class add 会直接报 File exists）。
	got := planClasses([]Shaped{
		{Mark: 0x120fb, Minor: 8443, BandwidthMbps: 50},
		{Mark: 0x120fb, Minor: 8443, BandwidthMbps: 50},
	})
	if len(got) != 1 {
		t.Errorf("重复 minor 应去重，实际生成 %d 个类", len(got))
	}
}

func TestParseDefaultIface(t *testing.T) {
	const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"单一物理网卡",
			"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n",
			"eth0",
		},
		{
			// 装了 Docker 的机器上 docker0 也可能带默认路由。
			// 给网桥限速是限不到真出口的，必须优先挑物理网卡。
			"物理网卡优先于 docker0",
			"docker0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"ens3\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			"ens3",
		},
		{
			// 但如果整台机器只有虚拟网卡带默认路由，限一下总比不限强。
			"只有虚拟网卡时退而求其次",
			"br-abc123\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			"br-abc123",
		},
		{
			"没有默认路由",
			"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n",
			"",
		},
		{"空表", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDefaultIface([]byte(header + tc.body)); got != tc.want {
				t.Errorf("parseDefaultIface = %q，期望 %q", got, tc.want)
			}
		})
	}
}
