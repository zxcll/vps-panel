package forward

import (
	"strings"
	"testing"
)

func TestPartitionSeparatesModes(t *testing.T) {
	kernel, userspace, err := Partition([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
		{HopID: 2, Proto: ProtoTCP, ListenPort: 9000, DestIP: "5.6.7.8", DestPort: 443, Mode: ModeUserspace},
		{HopID: 3, Proto: ProtoUDP, ListenPort: 5353, DestIP: "8.8.8.8", DestPort: 53},
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(kernel) != 2 || len(userspace) != 1 {
		t.Fatalf("内核态 %d 条、用户态 %d 条，期望 2 / 1", len(kernel), len(userspace))
	}
	if userspace[0].HopID != 2 {
		t.Errorf("用户态那条应该是 hop 2，实际 %d", userspace[0].HopID)
	}
}

func TestPartitionEmptyModeMeansKernel(t *testing.T) {
	// 空 Mode 必须落到内核态：老 state 文件和老面板的下发都没有这个字段。
	kernel, userspace, err := Partition([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443, Mode: ""},
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(kernel) != 1 || len(userspace) != 0 {
		t.Errorf("空 Mode 应归内核态，实际内核 %d / 用户 %d", len(kernel), len(userspace))
	}
}

func TestPartitionSplitsUserspaceTCPUDP(t *testing.T) {
	// 用户态不支持 UDP，所以 tcp+udp 要拆成「UDP 走内核 + TCP 走用户态」。
	kernel, userspace, err := Partition([]Rule{
		{HopID: 7, Proto: ProtoBoth, ListenPort: 7000, DestIP: "5.6.7.8", DestPort: 7000, Mode: ModeUserspace},
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(kernel) != 1 || kernel[0].Proto != ProtoUDP || kernel[0].Mode != ModeKernel {
		t.Errorf("内核那半应是 udp/kernel，实际 %+v", kernel)
	}
	if len(userspace) != 1 || userspace[0].Proto != ProtoTCP {
		t.Errorf("用户态那半应是 tcp，实际 %+v", userspace)
	}
	// 拆出来的两条必须还指向同一跳，否则计数会分家。
	if kernel[0].HopID != 7 || userspace[0].HopID != 7 {
		t.Errorf("拆分后 HopID 应保持为 7，实际 %d / %d", kernel[0].HopID, userspace[0].HopID)
	}
	// 目标和限速也要跟着复制过去。
	if kernel[0].DestIP != "5.6.7.8" || userspace[0].DestIP != "5.6.7.8" {
		t.Errorf("拆分后目标丢了：%+v / %+v", kernel[0], userspace[0])
	}
}

func TestPartitionDetectsPortConflicts(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
	}{
		{
			"同协议同端口",
			[]Rule{
				{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
				{HopID: 2, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2.2.2.2", DestPort: 2},
			},
		},
		{
			// 这一条是按协议字符串去重看不出来的："tcp+udp" != "tcp"，
			// 但它确实占了 tcp/8443。
			"tcp+udp 与 tcp 撞端口",
			[]Rule{
				{HopID: 1, Proto: ProtoBoth, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
				{HopID: 2, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2.2.2.2", DestPort: 2},
			},
		},
		{
			"内核态与用户态撞端口",
			[]Rule{
				{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
				{HopID: 2, Proto: ProtoTCP, ListenPort: 8443, DestIP: "2.2.2.2", DestPort: 2, Mode: ModeUserspace},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Partition(tc.rules)
			if err == nil {
				t.Fatal("端口冲突应该报错")
			}
			if !strings.Contains(err.Error(), "8443") {
				t.Errorf("错误消息里应该带上冲突的端口号，实际：%v", err)
			}
		})
	}
}

func TestPartitionAllowsSamePortDifferentProto(t *testing.T) {
	// tcp/8443 和 udp/8443 是两个不同的坑位，不算冲突。
	kernel, _, err := Partition([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: 8443, DestIP: "1.1.1.1", DestPort: 1},
		{HopID: 2, Proto: ProtoUDP, ListenPort: 8443, DestIP: "2.2.2.2", DestPort: 2},
	})
	if err != nil {
		t.Fatalf("同端口不同协议不该报错: %v", err)
	}
	if len(kernel) != 2 {
		t.Errorf("应有 2 条内核规则，实际 %d", len(kernel))
	}
}
