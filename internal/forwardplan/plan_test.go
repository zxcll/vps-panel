package forwardplan

import (
	"strings"
	"testing"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/store"
)

// 三台机器：A 是入口，B 是中转，C 是落地。
func testNodes() map[int64]*store.Node {
	return map[int64]*store.Node{
		1: {ID: 1, Name: "上海A", IPv4: "203.0.113.1", Enabled: true},
		2: {ID: 2, Name: "香港B", IPv4: "203.0.113.2", Enabled: true},
		3: {ID: 3, Name: "日本C", IPv4: "203.0.113.3", Enabled: true},
	}
}

func testFwdNodes() map[int64]*store.ForwardNode {
	return map[int64]*store.ForwardNode{
		1: store.DefaultForwardNode(1),
		2: store.DefaultForwardNode(2),
		3: store.DefaultForwardNode(3),
	}
}

func TestExpandSingleHop(t *testing.T) {
	r := &store.ForwardRule{
		ID: 10, Name: "直连", Proto: store.ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{
			{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443, Mode: store.ForwardModeKernel},
		},
	}
	got, err := Expand(r, testNodes(), testFwdNodes())
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应展开出 1 跳，实际 %d", len(got))
	}
	// 单跳规则的目标就是最终目标。
	if got[0].NodeID != 1 || got[0].Rule.DestIP != "1.2.3.4" || got[0].Rule.DestPort != 443 {
		t.Errorf("展开结果不对：%+v", got[0])
	}
	if got[0].Rule.HopID != 100 {
		t.Errorf("HopID 必须原样带过去（转发账本按它索引），实际 %d", got[0].Rule.HopID)
	}
}

func TestExpandThreeHopsChainsTargets(t *testing.T) {
	r := &store.ForwardRule{
		ID: 10, Name: "三跳", Proto: store.ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{
			{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443, Mode: store.ForwardModeKernel},
			{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001, Mode: store.ForwardModeUserspace},
			{ID: 102, Position: 2, NodeID: 3, ListenPort: 20002, Mode: store.ForwardModeKernel},
		},
	}
	got, err := Expand(r, testNodes(), testFwdNodes())
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应展开出 3 跳，实际 %d", len(got))
	}

	// A 指向 B 的监听端口。
	if got[0].Rule.DestIP != "203.0.113.2" || got[0].Rule.DestPort != 20001 {
		t.Errorf("第 1 跳应指向 B 的中继地址:20001，实际 %s:%d", got[0].Rule.DestIP, got[0].Rule.DestPort)
	}
	// B 指向 C 的监听端口。
	if got[1].Rule.DestIP != "203.0.113.3" || got[1].Rule.DestPort != 20002 {
		t.Errorf("第 2 跳应指向 C 的中继地址:20002，实际 %s:%d", got[1].Rule.DestIP, got[1].Rule.DestPort)
	}
	// 只有最后一跳指向真正的目标。
	if got[2].Rule.DestIP != "1.2.3.4" || got[2].Rule.DestPort != 443 {
		t.Errorf("最后一跳应指向最终目标，实际 %s:%d", got[2].Rule.DestIP, got[2].Rule.DestPort)
	}
	// 每跳的模式各自独立。
	if got[1].Rule.Mode != store.ForwardModeUserspace {
		t.Errorf("第 2 跳应保持用户态，实际 %q", got[1].Rule.Mode)
	}
}

func TestExpandSortsByPosition(t *testing.T) {
	// 数据库里的顺序不该被信任，展开必须按 position 排。
	r := &store.ForwardRule{
		ID: 10, Name: "乱序", Proto: store.ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{
			{ID: 102, Position: 2, NodeID: 3, ListenPort: 20002},
			{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443},
			{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001},
		},
	}
	got, err := Expand(r, testNodes(), testFwdNodes())
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got[0].NodeID != 1 || got[1].NodeID != 2 || got[2].NodeID != 3 {
		t.Errorf("没有按 position 排序：%d → %d → %d", got[0].NodeID, got[1].NodeID, got[2].NodeID)
	}
}

func TestExpandUsesExplicitRelayHost(t *testing.T) {
	// 探测地址可能是内网口，中继必须用显式配的公网地址。
	fn := testFwdNodes()
	fn[2].RelayHost = "hk.example.com"

	r := &store.ForwardRule{
		ID: 10, Name: "域名中继", Proto: store.ForwardProtoTCP,
		DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{
			{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443},
			{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001},
		},
	}
	got, err := Expand(r, testNodes(), fn)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	// 中继地址是域名时要走 DestHost，交给探针定期解析，不能塞进 DestIP。
	if got[0].Rule.DestHost != "hk.example.com" || got[0].Rule.DestIP != "" {
		t.Errorf("域名中继应填进 DestHost：host=%q ip=%q", got[0].Rule.DestHost, got[0].Rule.DestIP)
	}
}

func TestExpandFinalDestinationCanBeDomain(t *testing.T) {
	r := &store.ForwardRule{
		ID: 10, Name: "域名目标", Proto: store.ForwardProtoTCP,
		DestHost: "target.example.com", DestPort: 443, Enabled: true,
		Hops: []store.ForwardHop{{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443}},
	}
	got, err := Expand(r, testNodes(), testFwdNodes())
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got[0].Rule.DestHost != "target.example.com" || got[0].Rule.DestIP != "" {
		t.Errorf("域名目标应填进 DestHost：%+v", got[0].Rule)
	}
}

func TestExpandErrors(t *testing.T) {
	base := func() *store.ForwardRule {
		return &store.ForwardRule{
			ID: 10, Name: "规则", Proto: store.ForwardProtoTCP,
			DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
			Hops: []store.ForwardHop{
				{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443},
				{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001},
			},
		}
	}

	cases := []struct {
		name    string
		setup   func(r *store.ForwardRule, nodes map[int64]*store.Node, fn map[int64]*store.ForwardNode)
		wantErr string
	}{
		{
			"没有跳",
			func(r *store.ForwardRule, _ map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				r.Hops = nil
			},
			"没有配置任何节点",
		},
		{
			"节点不存在",
			func(_ *store.ForwardRule, nodes map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				delete(nodes, 2)
			},
			"已不存在",
		},
		{
			"节点被禁用",
			func(_ *store.ForwardRule, nodes map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				nodes[2].Enabled = false
			},
			"已被禁用",
		},
		{
			"节点关闭了转发",
			func(_ *store.ForwardRule, _ map[int64]*store.Node, fn map[int64]*store.ForwardNode) {
				fn[2].Enabled = false
			},
			"关闭了端口转发",
		},
		{
			// 中间跳的节点连地址都没有，上一跳无处可发。
			"中继节点没有地址",
			func(_ *store.ForwardRule, nodes map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				nodes[2].IPv4 = ""
				nodes[2].SSHHost = ""
			},
			"没有可用的中继地址",
		},
		{
			"同一节点在链里出现两次",
			func(r *store.ForwardRule, _ map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				r.Hops[1].NodeID = 1
			},
			"出现了两次",
		},
		{
			"跳本身不合法",
			func(r *store.ForwardRule, _ map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				r.Hops[0].ListenPort = 0
			},
			"监听端口",
		},
		{
			"UDP 配了用户态",
			func(r *store.ForwardRule, _ map[int64]*store.Node, _ map[int64]*store.ForwardNode) {
				r.Proto = store.ForwardProtoUDP
				r.Hops[0].Mode = store.ForwardModeUserspace
			},
			"UDP 不支持用户态",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, nodes, fn := base(), testNodes(), testFwdNodes()
			tc.setup(r, nodes, fn)
			_, err := Expand(r, nodes, fn)
			if err == nil {
				t.Fatalf("应当报错，含 %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误消息 %q 里应含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestBuildGroupsByNode(t *testing.T) {
	in := Inputs{
		Nodes: testNodes(), FwdNodes: testFwdNodes(),
		Rules: []*store.ForwardRule{
			{
				ID: 1, Name: "规则一", Proto: store.ForwardProtoTCP,
				DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
				Hops: []store.ForwardHop{
					{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443},
					{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001},
				},
			},
			{
				ID: 2, Name: "规则二", Proto: store.ForwardProtoUDP,
				DestHost: "8.8.8.8", DestPort: 53, Enabled: true,
				Hops: []store.ForwardHop{
					{ID: 102, Position: 0, NodeID: 1, ListenPort: 5353},
				},
			},
		},
	}
	p := Build(in)
	if len(p.Problems) != 0 {
		t.Fatalf("不该有问题规则：%+v", p.Problems)
	}
	// 节点 1 上有两条规则的跳，要合成一份规则集。
	if len(p.ByNode[1]) != 2 {
		t.Errorf("节点 1 应有 2 条规则，实际 %d", len(p.ByNode[1]))
	}
	if len(p.ByNode[2]) != 1 {
		t.Errorf("节点 2 应有 1 条规则，实际 %d", len(p.ByNode[2]))
	}
	// 节点 3 没参与，不该出现。
	if _, ok := p.ByNode[3]; ok {
		t.Error("没参与转发的节点不该出现在计划里")
	}
}

// 规则被禁用时，它涉及的节点仍然要收到一份（空的）规则集，
// 否则机器上的旧规则永远撤不掉。
func TestBuildKeepsEmptySlotForDisabledRule(t *testing.T) {
	p := Build(Inputs{
		Nodes: testNodes(), FwdNodes: testFwdNodes(),
		Rules: []*store.ForwardRule{
			{
				ID: 1, Name: "关掉的规则", Proto: store.ForwardProtoTCP,
				DestHost: "1.2.3.4", DestPort: 443, Enabled: false,
				Hops: []store.ForwardHop{{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443}},
			},
		},
	})
	rules, ok := p.ByNode[1]
	if !ok {
		t.Fatal("涉及的节点必须出现在计划里，才能把旧规则撤掉")
	}
	if len(rules) != 0 {
		t.Errorf("被禁用的规则不该产出内容，实际 %d 条", len(rules))
	}
}

// 一条坏规则不该拖垮同一台机器上其他规则的下发。
func TestBuildIsolatesBrokenRule(t *testing.T) {
	nodes := testNodes()
	nodes[3].Enabled = false

	p := Build(Inputs{
		Nodes: nodes, FwdNodes: testFwdNodes(),
		Rules: []*store.ForwardRule{
			{
				ID: 1, Name: "好规则", Proto: store.ForwardProtoTCP,
				DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
				Hops: []store.ForwardHop{{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443}},
			},
			{
				ID: 2, Name: "坏规则", Proto: store.ForwardProtoTCP,
				DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
				Hops: []store.ForwardHop{
					{ID: 101, Position: 0, NodeID: 1, ListenPort: 8444},
					{ID: 102, Position: 1, NodeID: 3, ListenPort: 20001},
				},
			},
		},
	})
	if len(p.Problems) != 1 || p.Problems[0].RuleID != 2 {
		t.Fatalf("应当只报告规则 2 有问题：%+v", p.Problems)
	}
	if p.Problems[0].RuleName != "坏规则" {
		t.Errorf("问题里要带规则名，方便用户定位，实际 %q", p.Problems[0].RuleName)
	}
	if len(p.ByNode[1]) != 1 || p.ByNode[1][0].HopID != 100 {
		t.Errorf("好规则应当照常下发，实际 %+v", p.ByNode[1])
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	// 同样的输入必须生成同样的输出，面板才能靠比对内容判断要不要重下发。
	in := Inputs{
		Nodes: testNodes(), FwdNodes: testFwdNodes(),
		Rules: []*store.ForwardRule{
			{
				ID: 1, Name: "b", Proto: store.ForwardProtoTCP, DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
				Hops: []store.ForwardHop{{ID: 200, Position: 0, NodeID: 1, ListenPort: 8444}},
			},
			{
				ID: 2, Name: "a", Proto: store.ForwardProtoTCP, DestHost: "1.2.3.4", DestPort: 443, Enabled: true,
				Hops: []store.ForwardHop{{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443}},
			},
		},
	}
	first := Build(in).ByNode[1]
	second := Build(in).ByNode[1]
	if len(first) != len(second) {
		t.Fatalf("两次展开条数不同：%d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("第 %d 条不一致：%+v vs %+v", i, first[i], second[i])
		}
	}
	// 按 HopID 排序，所以 100 在 200 前面。
	if first[0].HopID != 100 {
		t.Errorf("应按 HopID 排序，实际首条是 %d", first[0].HopID)
	}
}

func TestEntryAddress(t *testing.T) {
	r := &store.ForwardRule{
		ID: 1, Name: "规则", Proto: store.ForwardProtoTCP, DestHost: "1.2.3.4", DestPort: 443,
		Hops: []store.ForwardHop{
			// 故意把入口放在切片后面，验证是按 position 而不是切片顺序取的。
			{ID: 101, Position: 1, NodeID: 2, ListenPort: 20001},
			{ID: 100, Position: 0, NodeID: 1, ListenPort: 8443},
		},
	}
	if got := EntryAddress(r, testNodes(), testFwdNodes()); got != "203.0.113.1:8443" {
		t.Errorf("入口地址 = %q，期望 203.0.113.1:8443", got)
	}
}

func TestAllocPort(t *testing.T) {
	fn := store.DefaultForwardNode(1)

	got, err := AllocPort(fn, map[int]string{})
	if err != nil || got != store.DefaultForwardPortStart {
		t.Errorf("空占用时应取范围起点，实际 %d, err=%v", got, err)
	}

	used := map[int]string{20000: "tcp", 20001: "udp"}
	got, err = AllocPort(fn, used)
	if err != nil || got != 20002 {
		t.Errorf("应跳过已占用端口，实际 %d, err=%v", got, err)
	}

	// 同端口不同协议也一律避开：中间端口不值得精打细算，
	// 撞在一起只会让排查变难。
	narrow := &store.ForwardNode{NodeID: 1, PortStart: 30000, PortEnd: 30000, Enabled: true}
	if _, err := AllocPort(narrow, map[int]string{30000: "tcp"}); err == nil {
		t.Error("范围用满时应报错")
	}

	// 范围配歪了（起点大于终点）要回落到默认范围，而不是直接失败。
	broken := &store.ForwardNode{NodeID: 1, PortStart: 500, PortEnd: 100, Enabled: true}
	if got, err := AllocPort(broken, map[int]string{}); err != nil || got != store.DefaultForwardPortStart {
		t.Errorf("范围非法时应回落默认，实际 %d, err=%v", got, err)
	}
}

func TestRelayHostFallbackChain(t *testing.T) {
	n := &store.Node{ID: 1, Name: "节点", IPv4: "203.0.113.1", SSHHost: "ssh.example.com"}

	if got := RelayHost(&store.ForwardNode{RelayHost: "relay.example.com"}, n); got != "relay.example.com" {
		t.Errorf("显式配置优先级最高，实际 %q", got)
	}
	if got := RelayHost(store.DefaultForwardNode(1), n); got != "203.0.113.1" {
		t.Errorf("没配显式地址时用 ipv4，实际 %q", got)
	}
	n.IPv4 = ""
	if got := RelayHost(store.DefaultForwardNode(1), n); got != "ssh.example.com" {
		t.Errorf("没有 ipv4 时回落 ssh 地址，实际 %q", got)
	}
	n.SSHHost = ""
	if got := RelayHost(store.DefaultForwardNode(1), n); got != "" {
		t.Errorf("什么都没有时应返回空串，实际 %q", got)
	}
}

// forward.Rule 必须是可比较的：Build 的确定性测试和面板侧「内容变了才重下发」
// 都依赖直接用 == 比。加了切片字段就会在这里炸掉。
var _ = func() forward.Rule { var a, b forward.Rule; _ = a == b; return a }
