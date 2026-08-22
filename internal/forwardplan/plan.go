// Package forwardplan 把面板里「一条规则 + 一串跳」的逻辑描述，
// 展开成每台机器各自要执行的转发规则。
//
// 展开规则很简单，但边界条件不少，所以这里全是纯函数，方便单测：
//
//	第 i 跳的目标 = 第 i+1 跳所在节点的中继地址 : 第 i+1 跳的监听端口
//	最后一跳的目标 = 规则里填的最终目标
//	入口地址      = 第 0 跳所在节点的中继地址 : 第 0 跳的监听端口
//
// 一个 position 上可以有**多台**机器，但只允许入口（position 0）这样：
// 几台机器共用同一个入口端口，一起加进域名记录的候选列表，域名故障切换
// 切到哪一台转发都还通。中间跳必须是唯一的一台 —— 链路中段一旦分叉，
// 「上一跳该往哪发」就没有定义了。
//
// 下发单位是**节点**而不是规则：一台机器上可能同时有好几条规则的跳，
// 必须合成一份完整规则集整体下发。nftables 本来就是删表重建，
// 全量语义天然幂等，探针重连、面板重启、漏了一条指令都能靠下一次下发自愈。
package forwardplan

import (
	"fmt"
	"math/rand"
	"net"
	"sort"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/store"
)

// Plan 是一次展开的结果。
type Plan struct {
	// ByNode 是每个节点该执行的完整规则集。
	// 参与了转发的节点一定会出现在这里；某个节点上所有规则都被禁用时，
	// 它对应的值是空切片而不是缺失 —— 这样才能把它上面的旧规则撤干净。
	ByNode map[int64][]forward.Rule

	// Problems 收集展开不了的规则。一条坏规则不该拖垮整个节点的规则集，
	// 所以这里不是 error 而是一份清单，面板照着提示用户哪条没生效。
	Problems []Problem
}

// Problem 是一条规则没能展开的原因。
type Problem struct {
	RuleID   int64  `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Reason   string `json:"reason"`
}

// Inputs 是展开需要的全部上下文，一次性传进来避免在循环里查库。
type Inputs struct {
	Rules []*store.ForwardRule
	Nodes map[int64]*store.Node
	// FwdNodes 里没有的节点用 store.DefaultForwardNode 兜底。
	FwdNodes map[int64]*store.ForwardNode
}

// Build 展开所有规则。
//
// 只展开启用的规则；禁用的规则不产出任何东西，它占用的监听端口
// 会在下一次下发时被自然撤掉（全量覆盖语义）。
func Build(in Inputs) Plan {
	p := Plan{ByNode: map[int64][]forward.Rule{}}

	// 先给每个参与转发的节点建一个空槽。缺了这一步，
	// 一个节点上的规则全被禁用时它压根不会出现在 ByNode 里，
	// 于是也就收不到下发，旧规则会一直留在机器上转发。
	for _, r := range in.Rules {
		for _, h := range r.Hops {
			if _, ok := p.ByNode[h.NodeID]; !ok {
				p.ByNode[h.NodeID] = []forward.Rule{}
			}
		}
	}

	for _, r := range in.Rules {
		if !r.Enabled {
			continue
		}
		placed, err := Expand(r, in.Nodes, in.FwdNodes)
		if err != nil {
			p.Problems = append(p.Problems, Problem{RuleID: r.ID, RuleName: r.Name, Reason: err.Error()})
			continue
		}
		for _, pl := range placed {
			p.ByNode[pl.NodeID] = append(p.ByNode[pl.NodeID], pl.Rule)
		}
	}

	// 排序只为让下发内容稳定：同样的配置每次生成同样的字节，
	// 面板才能靠比对内容判断"要不要重下发"。
	for nodeID := range p.ByNode {
		rules := p.ByNode[nodeID]
		sort.Slice(rules, func(i, j int) bool { return rules[i].HopID < rules[j].HopID })
		p.ByNode[nodeID] = rules
	}
	sort.Slice(p.Problems, func(i, j int) bool { return p.Problems[i].RuleID < p.Problems[j].RuleID })
	return p
}

// Placed 是一条落在具体节点上的转发规则。
type Placed struct {
	NodeID int64
	// Position 是这一跳在链路里的位置，0 即入口。多入口时会有多条
	// Placed 共用 Position 0。
	Position int
	// NextNodeID 是这一跳把流量交给谁。0 表示它已经是最后一跳，
	// 直接拨落地目标。链路测试靠它描述「这一段是从哪到哪」。
	NextNodeID int64
	Rule       forward.Rule
}

// stage 是同一个 position 上的全部跳。只有入口那个 stage 允许有多条。
type stage struct {
	position int
	hops     []store.ForwardHop
}

// groupByPosition 把跳按 position 归拢成有序的 stage 列表。
//
// 数据库里的行序不该被信任，一律按 position 排；stage 内部按 NodeID 排，
// 保证同样的配置每次展开出同样的顺序（下发内容要能靠比对判断有没有变）。
func groupByPosition(hops []store.ForwardHop) []stage {
	byPos := map[int][]store.ForwardHop{}
	for _, h := range hops {
		byPos[h.Position] = append(byPos[h.Position], h)
	}

	positions := make([]int, 0, len(byPos))
	for p := range byPos {
		positions = append(positions, p)
	}
	sort.Ints(positions)

	out := make([]stage, 0, len(positions))
	for _, p := range positions {
		group := byPos[p]
		sort.Slice(group, func(i, j int) bool { return group[i].NodeID < group[j].NodeID })
		out = append(out, stage{position: p, hops: group})
	}
	return out
}

// Expand 把一条规则展开成每一跳的转发规则。
func Expand(r *store.ForwardRule, nodes map[int64]*store.Node, fwdNodes map[int64]*store.ForwardNode) ([]Placed, error) {
	if len(r.Hops) == 0 {
		return nil, fmt.Errorf("规则没有配置任何节点")
	}

	stages := groupByPosition(r.Hops)

	// 中间跳分叉的话，上一跳该往哪发就没有定义了。只有入口可以是多台机器。
	for i, st := range stages {
		if i > 0 && len(st.hops) > 1 {
			return nil, fmt.Errorf("只有入口可以配多个节点，第 %d 跳配了 %d 个", i+1, len(st.hops))
		}
	}

	// 同一个节点在一条链里出现两次几乎一定是配错了：那意味着流量绕回自己，
	// 而且「按 (规则, 节点) 定位一跳」这个前提也不成立了。
	seen := map[int64]bool{}
	for _, st := range stages {
		for _, h := range st.hops {
			if seen[h.NodeID] {
				name := nodeName(nodes, h.NodeID)
				return nil, fmt.Errorf("节点「%s」在这条链里出现了两次", name)
			}
			seen[h.NodeID] = true
		}
	}

	out := make([]Placed, 0, len(r.Hops))
	for i, st := range stages {
		// 这一跳交给谁：最后一个 stage 直接拨落地目标，否则拨下一个 stage
		// 那唯一一台机器。同一 stage 里的多个入口共用同一个下一跳。
		var destHost string
		var destPort int
		var nextNodeID int64
		if i == len(stages)-1 {
			destHost, destPort = r.DestHost, r.DestPort
		} else {
			next := stages[i+1].hops[0]
			// 先确认下一跳的节点还在。少了这一步，节点被删掉时报出来的是
			// 「没有可用的中继地址」，指向完全错误的排查方向。
			nextNode := nodes[next.NodeID]
			if nextNode == nil {
				return nil, fmt.Errorf("第 %d 跳指向的节点已不存在", i+2)
			}
			relay := RelayHost(fwdNodeOf(fwdNodes, next.NodeID), nextNode)
			if relay == "" {
				return nil, fmt.Errorf("节点「%s」没有可用的中继地址，请在转发设置里填写", nextNode.Name)
			}
			destHost, destPort, nextNodeID = relay, next.ListenPort, next.NodeID
		}

		for _, h := range st.hops {
			node := nodes[h.NodeID]
			if node == nil {
				return nil, fmt.Errorf("第 %d 跳指向的节点已不存在", i+1)
			}
			if !node.Enabled {
				return nil, fmt.Errorf("第 %d 跳的节点「%s」已被禁用", i+1, node.Name)
			}
			if fn := fwdNodeOf(fwdNodes, h.NodeID); !fn.Enabled {
				return nil, fmt.Errorf("节点「%s」关闭了端口转发", node.Name)
			}

			rule := forward.Rule{
				HopID:         h.ID,
				Proto:         r.Proto,
				ListenPort:    h.ListenPort,
				DestPort:      destPort,
				Mode:          h.Mode,
				BandwidthMbps: h.BandwidthMbps,
				Label:         hopDisplayLabel(r.Name, i, len(st.hops) > 1, node.Name),
			}
			// 是 IP 就直接填 DestIP，是域名才交给探针去解析。
			// 中间跳的中继地址通常是 IP，能省掉一次没必要的 DNS 查询。
			if net.ParseIP(destHost) != nil {
				rule.DestIP = destHost
			} else {
				rule.DestHost = destHost
			}

			if err := forward.Validate(rule); err != nil {
				return nil, fmt.Errorf("第 %d 跳不合法: %w", i+1, err)
			}
			out = append(out, Placed{
				NodeID:     h.NodeID,
				Position:   st.position,
				NextNodeID: nextNodeID,
				Rule:       rule,
			})
		}
	}
	return out, nil
}

// hopDisplayLabel 生成 nft 注释和日志里的那行描述。
// 多入口时带上机器名，否则两个入口在日志里长得一模一样，没法区分。
func hopDisplayLabel(ruleName string, index int, shared bool, nodeName string) string {
	if shared {
		return fmt.Sprintf("%s 第%d跳(%s)", ruleName, index+1, nodeName)
	}
	return fmt.Sprintf("%s 第%d跳", ruleName, index+1)
}

// RelayHost 返回其他节点访问这个节点用的地址。
//
// 优先级：转发设置里明确填的 relay_host > 节点的 ipv4 > SSH 地址。
// 之所以要独立于探测地址：探测可能走内网或管理口，而中转入口必须是
// 对上一跳可达的地址，两者未必是同一个。
func RelayHost(fn *store.ForwardNode, n *store.Node) string {
	if fn != nil && fn.RelayHost != "" {
		return fn.RelayHost
	}
	if n == nil {
		return ""
	}
	if n.IPv4 != "" {
		return n.IPv4
	}
	return n.SSHHost
}

// Entry 是一条规则的一个入口，也就是用户可以往哪里连。
type Entry struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	// Address 是 host:port 形式，前端直接给复制按钮。
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// Entries 返回一条规则的全部入口。
//
// 多个入口共用同一个端口，配合域名故障切换用：把这几台一起加进域名记录的
// 候选列表，域名切到哪一台，客户端连的地址端口都不用改。
//
// 拿不到中继地址的入口会被跳过 —— 那台机器根本没法被连上，列出来只会误导。
func Entries(r *store.ForwardRule, nodes map[int64]*store.Node, fwdNodes map[int64]*store.ForwardNode) []Entry {
	stages := groupByPosition(r.Hops)
	if len(stages) == 0 {
		return []Entry{}
	}

	out := make([]Entry, 0, len(stages[0].hops))
	for _, h := range stages[0].hops {
		host := RelayHost(fwdNodeOf(fwdNodes, h.NodeID), nodes[h.NodeID])
		if host == "" {
			continue
		}
		out = append(out, Entry{
			NodeID:   h.NodeID,
			NodeName: nodeName(nodes, h.NodeID),
			Address:  net.JoinHostPort(host, fmt.Sprintf("%d", h.ListenPort)),
			Port:     h.ListenPort,
		})
	}
	return out
}

// randIntn 做成变量是为了测试能固定住随机性。
var randIntn = rand.Intn

// allocRandomTries 是随机挑端口的尝试次数。范围通常有上万个坑位、
// 只占用了几个，随机几次几乎必中；试不中说明范围快满了，再走顺序扫描兜底。
const allocRandomTries = 16

// AllocPort 在节点配置的范围内挑一个还没被占用的端口。
//
// 随机取而不是从头顺序取：顺序取的话所有节点的第一条规则都会落在范围起点
// （比如清一色 20000），既不好认，也让端口变得可预测。
//
// used 是该节点上已占用的 端口 → 协议。同端口不同协议其实是两个坑位，
// 但这里一律避开：中间端口没必要精打细算，撞在一起只会让
// 「这个端口到底是谁的」变得难查。
func AllocPort(fn *store.ForwardNode, used map[int]string) (int, error) {
	start, end := portRangeOf(fn)
	return allocInRange(start, end, used)
}

// AllocSharedPort 挑一个在这一跳的**每台**机器上都空闲的端口。
//
// 多个入口必须共用同一个端口：端口不一样的话，域名故障切换一切到另一台，
// 客户端就得改配置，那这个功能也就没意义了。
//
// 做法是先求各节点端口范围的交集，再把各自的占用并起来，然后照常挑。
// fns 和 used 按下标一一对应。
func AllocSharedPort(fns []*store.ForwardNode, used []map[int]string) (int, error) {
	if len(fns) == 0 {
		return 0, fmt.Errorf("没有可分配端口的节点")
	}
	if len(fns) != len(used) {
		return 0, fmt.Errorf("节点数 %d 与占用表数 %d 对不上", len(fns), len(used))
	}

	// 交集：起点取最大、终点取最小。
	start, end := portRangeOf(fns[0])
	for _, fn := range fns[1:] {
		s, e := portRangeOf(fn)
		start = max(start, s)
		end = min(end, e)
	}
	if start > end {
		return 0, fmt.Errorf("这几台入口机器配置的端口范围没有交集，"+
			"请在节点的转发设置里把它们调成有重叠的范围（当前交集为 %d-%d）", start, end)
	}

	merged := map[int]string{}
	for _, u := range used {
		for port, proto := range u {
			merged[port] = proto
		}
	}
	return allocInRange(start, end, merged)
}

// portRangeOf 取节点配置的端口范围，没配过或配得不合法时回落默认范围。
func portRangeOf(fn *store.ForwardNode) (int, int) {
	if fn == nil {
		return store.DefaultForwardPortStart, store.DefaultForwardPortEnd
	}
	start, end := fn.PortStart, fn.PortEnd
	if start <= 0 || end <= 0 || start > end {
		return store.DefaultForwardPortStart, store.DefaultForwardPortEnd
	}
	return start, end
}

// allocInRange 在 [start, end] 里挑一个不在 used 里的端口。
func allocInRange(start, end int, used map[int]string) (int, error) {
	span := end - start + 1
	for range allocRandomTries {
		p := start + randIntn(span)
		if _, taken := used[p]; !taken {
			return p, nil
		}
	}
	// 随机没中，说明范围里剩的坑位不多了，老老实实扫一遍。
	for p := start; p <= end; p++ {
		if _, taken := used[p]; !taken {
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口范围 %d-%d 已经用满了，请在节点的转发设置里放宽范围", start, end)
}

func fwdNodeOf(m map[int64]*store.ForwardNode, nodeID int64) *store.ForwardNode {
	if fn, ok := m[nodeID]; ok && fn != nil {
		return fn
	}
	return store.DefaultForwardNode(nodeID)
}

func nodeName(nodes map[int64]*store.Node, id int64) string {
	if n := nodes[id]; n != nil {
		return n.Name
	}
	return fmt.Sprintf("#%d", id)
}
