// Package forwardplan 把面板里「一条规则 + 一串跳」的逻辑描述，
// 展开成每台机器各自要执行的转发规则。
//
// 展开规则很简单，但边界条件不少，所以这里全是纯函数，方便单测：
//
//	第 i 跳的目标 = 第 i+1 跳所在节点的中继地址 : 第 i+1 跳的监听端口
//	最后一跳的目标 = 规则里填的最终目标
//	入口地址      = 第 0 跳所在节点的中继地址 : 第 0 跳的监听端口
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
	Rule   forward.Rule
}

// Expand 把一条规则展开成每一跳的转发规则。
func Expand(r *store.ForwardRule, nodes map[int64]*store.Node, fwdNodes map[int64]*store.ForwardNode) ([]Placed, error) {
	if len(r.Hops) == 0 {
		return nil, fmt.Errorf("规则没有配置任何节点")
	}

	hops := append([]store.ForwardHop(nil), r.Hops...)
	sort.Slice(hops, func(i, j int) bool { return hops[i].Position < hops[j].Position })

	// 同一个节点在一条链里出现两次几乎一定是配错了：那意味着流量绕回自己，
	// 而且「按 (规则, 节点) 定位一跳」这个前提也不成立了。
	seen := map[int64]bool{}
	for _, h := range hops {
		if seen[h.NodeID] {
			name := nodeName(nodes, h.NodeID)
			return nil, fmt.Errorf("节点「%s」在这条链里出现了两次", name)
		}
		seen[h.NodeID] = true
	}

	out := make([]Placed, 0, len(hops))
	for i, h := range hops {
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

		var destHost string
		var destPort int
		if i == len(hops)-1 {
			destHost, destPort = r.DestHost, r.DestPort
		} else {
			next := hops[i+1]
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
			destHost, destPort = relay, next.ListenPort
		}

		rule := forward.Rule{
			HopID:         h.ID,
			Proto:         r.Proto,
			ListenPort:    h.ListenPort,
			DestPort:      destPort,
			Mode:          h.Mode,
			BandwidthMbps: h.BandwidthMbps,
			Label:         fmt.Sprintf("%s 第%d跳", r.Name, i+1),
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
		out = append(out, Placed{NodeID: h.NodeID, Rule: rule})
	}
	return out, nil
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

// EntryAddress 返回一条规则的入口地址，也就是用户该往哪里连。
func EntryAddress(r *store.ForwardRule, nodes map[int64]*store.Node, fwdNodes map[int64]*store.ForwardNode) string {
	if len(r.Hops) == 0 {
		return ""
	}
	first := r.Hops[0]
	for _, h := range r.Hops {
		if h.Position < first.Position {
			first = h
		}
	}
	host := RelayHost(fwdNodeOf(fwdNodes, first.NodeID), nodes[first.NodeID])
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", first.ListenPort))
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
	start, end := fn.PortStart, fn.PortEnd
	if start <= 0 || end <= 0 || start > end {
		start, end = store.DefaultForwardPortStart, store.DefaultForwardPortEnd
	}

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
