package server

import (
	"strings"
	"testing"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/forwardplan"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// 这一组用例守的是「本段延迟」和「拨通耗时」不能混为一谈。
//
// 背景：拨下一跳的**转发端口**量不到本段延迟。内核态转发在 PREROUTING 就把
// 目标地址改写并转发走了，下一跳自己的 TCP 栈根本不接这个连接 —— SYN-ACK 是
// 从落地目标回来的。所以那个数字是整条链路的往返，越靠前的跳越大。
//
// 真实例子（腾讯云 → 香港 → 落地，两跳都是内核态）：
//
//	落地马来西亚：第1跳 37ms，第2跳 31ms
//	落地台湾：    第1跳 25ms，第2跳 19ms
//	落地 HKT：    第1跳  8ms，第2跳  9ms
//
// 第一跳明明是同一台机器、同一条网络路径，数字却跟着落地目标变 ——
// 因为它量的从来就不是这一段。真实的腾讯云→香港只有 6ms 左右。

func testProbeNodes() map[int64]*store.Node {
	return map[int64]*store.Node{
		1: {ID: 1, Name: "腾讯云", IPv4: "123.207.42.3", SSHPort: 22, Enabled: true},
		2: {ID: 2, Name: "YT-HKIX", IPv4: "138.252.163.80", SSHPort: 22, Enabled: true},
		3: {ID: 3, Name: "第三跳", IPv4: "203.0.113.3", ProbePort: 8022, SSHPort: 22, Enabled: true},
	}
}

func testProbeFwdNodes() map[int64]*store.ForwardNode {
	return map[int64]*store.ForwardNode{
		1: store.DefaultForwardNode(1),
		2: store.DefaultForwardNode(2),
		3: store.DefaultForwardNode(3),
	}
}

// 中间跳要拨下一跳机器上一个会在本地终结的端口，而不是转发端口。
func TestHopRTTTargetUsesNextHopProbePort(t *testing.T) {
	p := forwardplan.Placed{
		NodeID: 1, Position: 0, NextNodeID: 2,
		Rule: forward.Rule{ListenPort: 27849, DestIP: "138.252.163.80", DestPort: 26230},
	}

	got := hopRTTTarget(p, testProbeNodes(), testProbeFwdNodes())
	if got != "138.252.163.80:22" {
		t.Errorf("应拨下一跳的探测端口 138.252.163.80:22，实际 %q", got)
	}
	// 关键：**不能**是转发端口 —— 那个端口会被 DNAT 转走，量到的是整条链路。
	if strings.Contains(got, "26230") {
		t.Error("拨了转发端口，量到的会是整条链路的往返而不是本段")
	}
}

// 节点配了独立的探测端口就用它，别硬用 SSH 端口。
func TestHopRTTTargetHonoursProbePort(t *testing.T) {
	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 3}
	if got := hopRTTTarget(p, testProbeNodes(), testProbeFwdNodes()); got != "203.0.113.3:8022" {
		t.Errorf("应用节点配的探测端口 8022，实际 %q", got)
	}
}

// 中继地址是显式配的域名时也要跟着走 —— 探测端口在同一台机器上。
func TestHopRTTTargetUsesRelayHost(t *testing.T) {
	fn := testProbeFwdNodes()
	fn[2].RelayHost = "hk.example.com"

	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 2}
	if got := hopRTTTarget(p, testProbeNodes(), fn); got != "hk.example.com:22" {
		t.Errorf("应跟着中继地址走，实际 %q", got)
	}
}

// 最后一跳不用单独测：它拨的落地目标本来就会终结连接。
func TestHopRTTTargetEmptyForLastHop(t *testing.T) {
	p := forwardplan.Placed{NodeID: 2, Position: 1, NextNodeID: 0}
	if got := hopRTTTarget(p, testProbeNodes(), testProbeFwdNodes()); got != "" {
		t.Errorf("最后一跳不该再测本段延迟，实际 %q", got)
	}
}

func TestSegmentLatencyLastHopUsesDialTime(t *testing.T) {
	p := forwardplan.Placed{NodeID: 2, Position: 1, NextNodeID: 0}
	probe := &protocol.ForwardProbe{LatencyMS: 31}

	ms, note := segmentLatency(p, probe, "")
	if ms != 31 {
		t.Errorf("最后一跳的握手耗时就是本段延迟，应为 31，实际 %d", ms)
	}
	if note != "" {
		t.Errorf("最后一跳不该有说明，实际 %q", note)
	}
}

// 核心用例：中间跳的本段延迟取的是单独量的 RTT，不是拨通耗时。
func TestSegmentLatencyIgnoresCumulativeDialTime(t *testing.T) {
	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 2}
	// 拨通耗时 37ms（含下游），单独量到的本段只有 6ms。
	probe := &protocol.ForwardProbe{LatencyMS: 37, RTTMS: 6}

	ms, note := segmentLatency(p, probe, "138.252.163.80:22")
	if ms != 6 {
		t.Errorf("本段延迟应取单独量的 6ms，实际 %d —— 取了 37ms 就等于把下游算进来了", ms)
	}
	if note != "" {
		t.Errorf("量到了就不该有说明，实际 %q", note)
	}
}

// 老探针不认识 rtt_target，回执里 RTTMS 是 0。这时候要说清楚为什么没数字，
// 而不是显示成 0ms 让人以为这一段是零延迟。
func TestSegmentLatencyExplainsOldAgent(t *testing.T) {
	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 2}
	probe := &protocol.ForwardProbe{LatencyMS: 37}

	ms, note := segmentLatency(p, probe, "138.252.163.80:22")
	if ms != 0 {
		t.Errorf("没量到就该是 0，实际 %d", ms)
	}
	if !strings.Contains(note, "升级探针") {
		t.Errorf("应提示升级探针，实际 %q", note)
	}
}

// 探测端口被防火墙静默丢弃时，说清楚原因，别让人以为链路有问题。
func TestSegmentLatencyExplainsProbeFailure(t *testing.T) {
	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 2}
	probe := &protocol.ForwardProbe{
		LatencyMS: 37,
		RTTError:  "dial tcp 138.252.163.80:22: i/o timeout",
	}

	ms, note := segmentLatency(p, probe, "138.252.163.80:22")
	if ms != 0 {
		t.Errorf("量不到就该是 0，实际 %d", ms)
	}
	if !strings.Contains(note, "防火墙") {
		t.Errorf("说明里应提到防火墙这个最可能的原因，实际 %q", note)
	}
	// 量不到本段延迟不影响连通性结论，说明里别用「不通」这种吓人的词。
	if strings.Contains(note, "不通") {
		t.Errorf("量不到延迟 ≠ 链路不通，说明措辞要小心：%q", note)
	}
}

// 下一跳连中继地址都没有时，如实说明。
func TestSegmentLatencyExplainsMissingRelay(t *testing.T) {
	p := forwardplan.Placed{NodeID: 1, Position: 0, NextNodeID: 2}
	probe := &protocol.ForwardProbe{LatencyMS: 37}

	_, note := segmentLatency(p, probe, "")
	if !strings.Contains(note, "中继地址") {
		t.Errorf("应说明是缺中继地址，实际 %q", note)
	}
}
