package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/forwardplan"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// panelDialTimeout 是面板自己拨入口地址的超时。
const panelDialTimeout = 6 * time.Second

// forwardTestReport 是一次链路测试的完整结果。
//
// 设计意图：链路不通时，「哪一段断了」比「通不通」重要得多。所以这里逐跳
// 单独测，每一跳都让它所在的机器去拨自己的下一跳 —— 这样能直接指出
// 是第几段出的问题，而不是让用户从入口开始一段段猜。
type forwardTestReport struct {
	RuleID   int64  `json:"rule_id"`
	RuleName string `json:"rule_name"`
	OK       bool   `json:"ok"`
	// Summary 是一句话结论，前端直接显示。
	Summary string `json:"summary"`
	// Entry 是面板自己拨入口地址的结果，可能为 nil（规则没有可用入口地址）。
	Entry *forwardTestLeg  `json:"entry,omitempty"`
	Legs  []forwardTestLeg `json:"legs"`
}

// forwardTestLeg 是链路上的一段。
type forwardTestLeg struct {
	// Position 从 0 开始，-1 表示这是「面板 → 入口」那一段。
	Position  int    `json:"position"`
	From      string `json:"from"`
	Target    string `json:"target"`
	OK        bool   `json:"ok"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	// Diagnosis 是这台机器的转发能力体检，只在从节点发起的段上有值。
	Diagnosis *legDiagnosis `json:"diagnosis,omitempty"`
}

// legDiagnosis 是节点侧的自检结果，用来把「连不上」归因到具体原因。
type legDiagnosis struct {
	NftAvailable bool     `json:"nft_available"`
	IPForward    bool     `json:"ip_forward"`
	Listening    bool     `json:"listening"`
	Firewalls    []string `json:"firewalls,omitempty"`
	RuleCount    int      `json:"rule_count"`
	// Problems 是体检发现的问题，已经翻译成人话。
	Problems []string `json:"problems,omitempty"`
}

// handleTestForward 测一条转发规则的整条链路。
func (s *Server) handleTestForward(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	ctx, cancel := actionContext(r.Context())
	defer cancel()

	rule, err := s.st.GetForwardRule(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	report, err := s.testForwardRule(ctx, rule)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) testForwardRule(ctx context.Context, rule *store.ForwardRule) (*forwardTestReport, error) {
	nodeList, err := s.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		return nil, err
	}
	nodes := make(map[int64]*store.Node, len(nodeList))
	for _, n := range nodeList {
		nodes[n.ID] = n
	}

	// 复用下发时那套展开逻辑，保证测的就是机器上实际跑的东西。
	// 展开都失败的话根本没到能测连通性这一步，直接把原因回去。
	placed, err := forwardplan.Expand(rule, nodes, fwdNodes)
	if err != nil {
		return &forwardTestReport{
			RuleID: rule.ID, RuleName: rule.Name, OK: false,
			Summary: "规则没能生效：" + err.Error(),
			Legs:    []forwardTestLeg{},
		}, nil
	}

	report := &forwardTestReport{RuleID: rule.ID, RuleName: rule.Name, Legs: []forwardTestLeg{}}

	// 逐跳测：让每台机器去拨它自己的下一跳。
	for i, p := range placed {
		nodeName := "节点"
		if n := nodes[p.NodeID]; n != nil {
			nodeName = n.Name
		}
		leg := forwardTestLeg{
			Position: i,
			From:     nodeName,
			Target:   legTarget(p.Rule),
		}

		probe, err := s.probeFromNode(ctx, p.NodeID, leg.Target, p.Rule.ListenPort)
		switch {
		case err != nil:
			leg.Error = err.Error()
		default:
			leg.OK = probe.OK
			leg.LatencyMS = probe.LatencyMS
			leg.Error = probe.Error
			leg.Diagnosis = diagnose(probe, p.Rule)
		}
		report.Legs = append(report.Legs, leg)
	}

	// 面板自己也拨一次入口。它和用户的网络路径未必相同，所以只作参考，
	// 不参与整体结论 —— 面板常常部署在内网，拨不通不代表用户拨不通。
	if entry := forwardplan.EntryAddress(rule, nodes, fwdNodes); entry != "" {
		report.Entry = s.dialFromPanel(ctx, entry)
	}

	report.OK, report.Summary = summarize(rule, report)
	return report, nil
}

// probeFromNode 让某个节点拨一次目标地址。
func (s *Server) probeFromNode(ctx context.Context, nodeID int64, target string, listenPort int) (*protocol.ForwardProbe, error) {
	if !s.hub.Online(nodeID) {
		return nil, fmt.Errorf("探针未连接，无法从这台机器发起测试")
	}
	cmd := protocol.Command{
		ID:         action.NewCommandID(nodeID),
		Cmd:        protocol.CmdForwardTest,
		TimeoutSec: 15,
		Reason:     "面板发起的转发链路测试",
		Probe:      &protocol.ProbeRequest{Target: target, ListenPort: listenPort},
	}
	res, err := s.hub.Send(ctx, nodeID, cmd)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("%s", res.Error)
	}
	var probe protocol.ForwardProbe
	if err := json.Unmarshal([]byte(res.Output), &probe); err != nil {
		return nil, fmt.Errorf("探针回执解析失败（探针版本可能过旧）: %w", err)
	}
	return &probe, nil
}

// dialFromPanel 面板自己拨一次地址。
func (s *Server) dialFromPanel(ctx context.Context, target string) *forwardTestLeg {
	leg := &forwardTestLeg{Position: -1, From: "面板", Target: target}
	start := time.Now()
	d := net.Dialer{Timeout: panelDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	leg.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		leg.Error = err.Error()
		return leg
	}
	conn.Close()
	leg.OK = true
	return leg
}

// legTarget 取一条展开后规则的目标地址。
func legTarget(r forward.Rule) string {
	host := r.DestHost
	if host == "" {
		host = r.DestIP
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", r.DestPort))
}

// diagnose 把探针的体检结果翻译成人能看懂的问题清单。
func diagnose(p *protocol.ForwardProbe, r forward.Rule) *legDiagnosis {
	d := &legDiagnosis{
		NftAvailable: p.NftAvailable,
		IPForward:    p.IPForward,
		Listening:    p.Listening,
		Firewalls:    p.Firewalls,
		RuleCount:    p.RuleCount,
	}
	kernel := r.EffectiveMode() == forward.ModeKernel

	if kernel && !p.NftAvailable {
		d.Problems = append(d.Problems,
			"这台机器上没有 nftables，内核态转发不可能生效。装一下 nftables 包，或者把这一跳改成用户态。")
	}
	if kernel && !p.IPForward {
		// 这是最容易被误判成"防火墙拦了"的一种：规则都在、就是不通。
		d.Problems = append(d.Problems,
			"内核转发开关（net.ipv4.ip_forward）没开，DNAT 之后的包会被内核直接丢掉。"+
				"探针下发规则时会自动开启，还是这样的话多半是容器里权限不够。")
	}
	if !kernel && !p.Listening {
		d.Problems = append(d.Problems,
			fmt.Sprintf("用户态转发要靠监听进程接活，但这台机器的 %d 端口上没人在听，说明规则没真正生效。", r.ListenPort))
	}
	if p.RuleCount == 0 {
		d.Problems = append(d.Problems,
			"探针上一条转发规则都没有，面板下发可能没送到。点「重新下发」再试。")
	}
	return d
}

// summarize 给出整体结论。
//
// 结论只看节点之间那几段：面板自己拨入口失败不算数——面板常常只监听在内网，
// 或者和用户走的根本不是同一条网络路径，拿它当判据会造成大量假警报。
func summarize(rule *store.ForwardRule, rep *forwardTestReport) (bool, string) {
	if !rule.Enabled {
		return false, "这条规则处于停用状态，机器上不会有对应的转发。"
	}
	if len(rep.Legs) == 0 {
		return false, "规则没有配置任何节点。"
	}

	for _, leg := range rep.Legs {
		if leg.OK {
			continue
		}
		who := fmt.Sprintf("第 %d 跳（%s）", leg.Position+1, leg.From)
		reason := leg.Error
		if leg.Diagnosis != nil && len(leg.Diagnosis.Problems) > 0 {
			reason = strings.Join(leg.Diagnosis.Problems, " ")
		}
		if reason == "" {
			reason = "连不上下一跳"
		}
		return false, fmt.Sprintf("链路在%s断了：%s", who, reason)
	}

	msg := fmt.Sprintf("链路全通，共 %d 跳。", len(rep.Legs))
	if rep.Entry != nil && !rep.Entry.OK {
		// 面板拨不通但每一跳都通，最常见的原因是云厂商的安全组没放行入口端口。
		msg += "不过面板自己拨入口地址没成功——如果客户端也连不上，多半是云服务商的" +
			"安全组/防火墙没放行入口端口（这一层在机器外面，探针改不了）。"
	}
	return true, msg
}
