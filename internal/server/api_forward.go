package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/forwardplan"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/resolver"
	"github.com/zxcll/vps-panel/internal/store"
)

// --- 视图 ---

type forwardRuleView struct {
	*store.ForwardRule
	// Entries 是用户可以连的全部入口。多入口时前端逐个列出来，
	// 每个都能复制 —— 它们共用同一个端口，配合域名切换用。
	Entries    []forwardplan.Entry `json:"entries"`
	ProtoLabel string              `json:"proto_label"`
	HopViews   []forwardHopView    `json:"hop_views"`
	TotalUp    int64               `json:"total_up"`
	TotalDown  int64               `json:"total_down"`
	// Problem 非空表示这条规则展开不了，没有真正生效。
	Problem string `json:"problem,omitempty"`
}

type forwardHopView struct {
	store.ForwardHop
	ModeLabel string `json:"mode_label"`
	// NodeOnline 用的是节点的状态字段，和「节点管理」页显示的是同一个东西。
	//
	// 以前这里直接取 hub.Online()，也就是「WebSocket 连着没有」。那个信号
	// 恢复得比心跳慢得多（探针降级 HTTP 时它一直是假），于是同一台机器在
	// 节点管理页是绿的、在这里是离线，看着像面板自相矛盾。
	NodeOnline bool `json:"node_online"`
	// NodeCommandable 才是「现在能不能给它下发指令」——下发转发规则、
	// 跑链路测试、推送升级都要长连接。它为假时页面上提示「等待长连接」，
	// 而不是说成离线。
	NodeCommandable bool  `json:"node_commandable"`
	UpBytes         int64 `json:"up_bytes"`
	DownBytes       int64 `json:"down_bytes"`
	// NodeShare 是这一跳**实际消耗掉**的机器流量（界面上就叫这个名字）。
	// 一份流量经过中转机要进来一次再出去一次，所以双向计费口径下是 (up+down) 的两倍。
	// 有了它，用户才能把「规则转了 50G」和「机器用了 100G」对上号。
	NodeShare int64 `json:"node_share"`
	// Target 是这一跳实际转去哪，展示用。
	Target string `json:"target"`
}

type forwardNodeView struct {
	*store.ForwardNode
	NodeName string `json:"node_name"`
	// EffectiveRelayHost 是回落之后真正会用的中继地址，
	// 用户不用自己推「留空到底会用哪个」。
	EffectiveRelayHost string `json:"effective_relay_host"`
	Online             bool   `json:"online"`
	// UsedPorts 是该节点上已被转发占用的端口数，帮用户判断范围够不够。
	UsedPorts int `json:"used_ports"`
}

func forwardProtoLabel(p string) string {
	switch p {
	case store.ForwardProtoUDP:
		return "UDP"
	case store.ForwardProtoBoth:
		return "TCP + UDP"
	default:
		return "TCP"
	}
}

func forwardModeLabel(m string) string {
	if m == store.ForwardModeUserspace {
		return "用户态（分段 TCP）"
	}
	return "内核态（零拷贝）"
}

// --- 请求 ---

type forwardHopRequest struct {
	// NodeIDs 是这一跳落在哪些机器上。
	//
	// 只有入口（第一跳）允许填多个：把几台机器都设成入口，再一起加进
	// 域名记录的候选列表，域名切到哪一台转发都还通。中间跳必须只有一个 ——
	// 链路中段如果分叉，上一跳该往哪发就没有定义了。
	NodeIDs []int64 `json:"node_ids"`
	// ListenPort 为 0 表示让面板从节点配置的端口范围里随机挑一个。
	// 多个入口共用这一个端口 —— 端口不一样的话，域名一切换客户端就连不上了。
	ListenPort    int    `json:"listen_port"`
	Mode          string `json:"mode"`
	BandwidthMbps int    `json:"bandwidth_mbps"`
}

type forwardRequest struct {
	Name     string              `json:"name"`
	Proto    string              `json:"proto"`
	DestHost string              `json:"dest_host"`
	DestPort int                 `json:"dest_port"`
	Enabled  bool                `json:"enabled"`
	Remark   string              `json:"remark"`
	Hops     []forwardHopRequest `json:"hops"`
}

// validate 校验并顺手做归一化。错误消息直接显示给用户，所以要带上出错的值。
func (req *forwardRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fmt.Errorf("规则名称不能为空")
	}

	if req.Proto == "" {
		req.Proto = store.ForwardProtoTCP
	}
	switch req.Proto {
	case store.ForwardProtoTCP, store.ForwardProtoUDP, store.ForwardProtoBoth:
	default:
		return fmt.Errorf("协议 %q 不合法，只能是 tcp、udp 或 tcp+udp", req.Proto)
	}

	req.DestHost = strings.TrimSpace(req.DestHost)
	if req.DestHost == "" {
		return fmt.Errorf("目标地址不能为空")
	}
	if net.ParseIP(req.DestHost) == nil && !resolver.PlausibleHostname(req.DestHost) {
		return fmt.Errorf("目标地址 %q 既不是合法 IP 也不是合法域名", req.DestHost)
	}
	if req.DestPort < 1 || req.DestPort > 65535 {
		return fmt.Errorf("目标端口 %d 不合法，应在 1-65535 之间", req.DestPort)
	}

	if len(req.Hops) == 0 {
		return fmt.Errorf("至少要选一个中转节点")
	}

	seen := map[int64]bool{}
	for i := range req.Hops {
		h := &req.Hops[i]
		if len(h.NodeIDs) == 0 {
			return fmt.Errorf("第 %d 跳没有选择节点", i+1)
		}
		if i > 0 && len(h.NodeIDs) > 1 {
			return fmt.Errorf("只有入口可以配多个节点，第 %d 跳配了 %d 个", i+1, len(h.NodeIDs))
		}
		for _, id := range h.NodeIDs {
			if id <= 0 {
				return fmt.Errorf("第 %d 跳的节点 ID 不合法", i+1)
			}
			if seen[id] {
				return fmt.Errorf("同一个节点不能在链路里出现两次")
			}
			seen[id] = true
		}

		// 0 表示让面板从节点配置的端口范围里随机挑一个，入口跳也支持。
		if h.ListenPort != 0 && (h.ListenPort < 1 || h.ListenPort > 65535) {
			return fmt.Errorf("第 %d 跳的监听端口 %d 不合法，应在 1-65535 之间（填 0 表示自动分配）", i+1, h.ListenPort)
		}

		if h.Mode == "" {
			h.Mode = store.ForwardModeKernel
		}
		switch h.Mode {
		case store.ForwardModeKernel, store.ForwardModeUserspace:
		default:
			return fmt.Errorf("第 %d 跳的转发模式 %q 不合法", i+1, h.Mode)
		}
		// 提前拦下来，别等下发到机器上才失败。
		if h.Mode == store.ForwardModeUserspace && req.Proto == store.ForwardProtoUDP {
			return fmt.Errorf("第 %d 跳选了用户态转发，但 UDP 不支持用户态，请改成内核态", i+1)
		}
		if h.BandwidthMbps < 0 {
			return fmt.Errorf("第 %d 跳的限速 %d 不合法，应为正整数或 0（不限）", i+1, h.BandwidthMbps)
		}
	}
	return nil
}

// applyTo 把请求写进模型，并给留空的中间跳分配端口。
func (s *Server) applyForwardRequest(ctx context.Context, req *forwardRequest, r *store.ForwardRule) error {
	r.Name = req.Name
	r.Proto = req.Proto
	r.DestHost = req.DestHost
	r.DestPort = req.DestPort
	r.Enabled = req.Enabled
	r.Remark = strings.TrimSpace(req.Remark)

	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		return fmt.Errorf("读取节点转发配置: %w", err)
	}

	hops := make([]store.ForwardHop, 0, len(req.Hops))
	for i, h := range req.Hops {
		port := h.ListenPort
		if port == 0 {
			// 这一跳的所有节点共用一个端口，所以要挑一个在它们身上都空闲的。
			fns := make([]*store.ForwardNode, 0, len(h.NodeIDs))
			usedPerNode := make([]map[int]string, 0, len(h.NodeIDs))
			for _, id := range h.NodeIDs {
				used, err := s.st.UsedForwardPorts(ctx, id, r.ID)
				if err != nil {
					return fmt.Errorf("查询节点已占用端口: %w", err)
				}
				// 本次请求里用户手填的端口也要避开：库里还没有它们，
				// 但保存时会一起写进去，撞上就会被唯一约束打回来。
				for _, other := range req.Hops {
					if other.ListenPort <= 0 {
						continue
					}
					for _, oid := range other.NodeIDs {
						if oid == id {
							used[other.ListenPort] = "pending"
						}
					}
				}
				fn := fwdNodes[id]
				if fn == nil {
					fn = store.DefaultForwardNode(id)
				}
				fns = append(fns, fn)
				usedPerNode = append(usedPerNode, used)
			}
			var err error
			port, err = forwardplan.AllocSharedPort(fns, usedPerNode)
			if err != nil {
				return fmt.Errorf("第 %d 跳自动分配端口失败: %w", i+1, err)
			}
		}
		// 同一跳的每个节点各写一行，共用位置、端口、模式和限速。
		for _, id := range h.NodeIDs {
			hops = append(hops, store.ForwardHop{
				NodeID:        id,
				Position:      i,
				ListenPort:    port,
				Mode:          h.Mode,
				BandwidthMbps: h.BandwidthMbps,
			})
		}
	}
	r.Hops = hops
	return nil
}

// --- 规则 CRUD ---

func (s *Server) handleListForwards(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	views, err := s.forwardViews(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleGetForward(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	views, err := s.forwardViews(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	for _, v := range views {
		if v.ID == id {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, "转发规则不存在")
}

func (s *Server) handleCreateForward(w http.ResponseWriter, r *http.Request) {
	var req forwardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	if err := s.checkForwardNodes(ctx, req.Hops); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rule := &store.ForwardRule{}
	if err := s.applyForwardRequest(ctx, &req, rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.CreateForwardRule(ctx, rule); err != nil {
		writeForwardStoreErr(w, err)
		return
	}

	s.st.AddEvent(ctx, nil, store.EventForwardApply, store.LevelInfo,
		fmt.Sprintf("转发规则「%s」已创建", rule.Name))
	s.syncForwardAsync(ctx)

	full, err := s.st.GetForwardRule(ctx, rule.ID)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, full)
}

func (s *Server) handleUpdateForward(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req forwardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	if err := s.checkForwardNodes(ctx, req.Hops); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rule, err := s.st.GetForwardRule(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.applyForwardRequest(ctx, &req, rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.UpdateForwardRule(ctx, rule); err != nil {
		writeForwardStoreErr(w, err)
		return
	}

	s.st.AddEvent(ctx, nil, store.EventForwardApply, store.LevelInfo,
		fmt.Sprintf("转发规则「%s」已修改", rule.Name))
	s.syncForwardAsync(ctx)

	full, err := s.st.GetForwardRule(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, full)
}

func (s *Server) handleDeleteForward(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	rule, err := s.st.GetForwardRule(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.st.DeleteForwardRule(ctx, id); err != nil {
		handleStoreErr(w, err)
		return
	}

	s.st.AddEvent(ctx, nil, store.EventForwardApply, store.LevelInfo,
		fmt.Sprintf("转发规则「%s」已删除", rule.Name))
	// 删完必须立刻下发，否则机器上的监听还开着。
	s.syncForwardAsync(ctx)

	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

type forwardToggleRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleToggleForward(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req forwardToggleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if err := s.st.SetForwardRuleEnabled(ctx, id, req.Enabled); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.syncForwardAsync(ctx)

	full, err := s.st.GetForwardRule(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// handleSyncForward 是面板上的「重新下发」按钮。
// 强制重发，不管面板记的版本号 —— 用户点它通常正是因为怀疑机器上的状态对不上。
func (s *Server) handleSyncForward(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r.Context())
	defer cancel()

	s.SyncForward(ctx, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"message":  "已重新下发",
		"problems": s.fwdSync.Problems(),
	})
}

// --- 节点的转发配置 ---

func (s *Server) handleListForwardNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	out := []forwardNodeView{}
	for _, n := range nodes {
		fn := fwdNodes[n.ID]
		if fn == nil {
			fn = store.DefaultForwardNode(n.ID)
		}
		used, err := s.st.UsedForwardPorts(ctx, n.ID, 0)
		if err != nil {
			handleStoreErr(w, err)
			return
		}
		out = append(out, forwardNodeView{
			ForwardNode:        fn,
			NodeName:           n.Name,
			EffectiveRelayHost: forwardplan.RelayHost(fn, n),
			Online:             s.hub.Online(n.ID),
			UsedPorts:          len(used),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type forwardNodeRequest struct {
	RelayHost   string `json:"relay_host"`
	RelayHostV6 string `json:"relay_host_v6"`
	PortStart   int    `json:"port_start"`
	PortEnd     int    `json:"port_end"`
	Enabled     bool   `json:"enabled"`
}

func (req *forwardNodeRequest) validate() error {
	req.RelayHost = strings.TrimSpace(req.RelayHost)
	req.RelayHostV6 = strings.TrimSpace(req.RelayHostV6)

	// 中继地址允许填 IP 或域名，留空表示回落节点的 ipv4。
	if req.RelayHost != "" && net.ParseIP(req.RelayHost) == nil && !resolver.PlausibleHostname(req.RelayHost) {
		return fmt.Errorf("中继地址 %q 既不是合法 IP 也不是合法域名", req.RelayHost)
	}
	if req.RelayHostV6 != "" && net.ParseIP(req.RelayHostV6) == nil {
		return fmt.Errorf("IPv6 中继地址 %q 不是合法 IP", req.RelayHostV6)
	}

	if req.PortStart == 0 && req.PortEnd == 0 {
		req.PortStart, req.PortEnd = store.DefaultForwardPortStart, store.DefaultForwardPortEnd
	}
	if req.PortStart < 1 || req.PortStart > 65535 {
		return fmt.Errorf("端口范围起点 %d 不合法", req.PortStart)
	}
	if req.PortEnd < 1 || req.PortEnd > 65535 {
		return fmt.Errorf("端口范围终点 %d 不合法", req.PortEnd)
	}
	if req.PortStart > req.PortEnd {
		return fmt.Errorf("端口范围起点 %d 大于终点 %d", req.PortStart, req.PortEnd)
	}
	return nil
}

func (s *Server) handleSaveForwardNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	var req forwardNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	fn := &store.ForwardNode{
		NodeID:      n.ID,
		RelayHost:   req.RelayHost,
		RelayHostV6: req.RelayHostV6,
		PortStart:   req.PortStart,
		PortEnd:     req.PortEnd,
		Enabled:     req.Enabled,
	}
	if err := s.st.SaveForwardNode(ctx, fn); err != nil {
		handleStoreErr(w, err)
		return
	}
	// 中继地址变了会改变上一跳的目标，必须重新下发整个链路。
	s.syncForwardAsync(ctx)

	writeJSON(w, http.StatusOK, forwardNodeView{
		ForwardNode:        fn,
		NodeName:           n.Name,
		EffectiveRelayHost: forwardplan.RelayHost(fn, n),
		Online:             s.hub.Online(n.ID),
	})
}

// --- 转发流量曲线 ---

func (s *Server) handleForwardTraffic(w http.ResponseWriter, r *http.Request) {
	hopID, ok := pathID(w, r)
	if !ok {
		return
	}
	hours := atoiDefault(r.URL.Query().Get("hours"), 48)
	if hours < 1 || hours > 24*90 {
		hours = 48
	}
	to := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	from := to.Add(-time.Duration(hours) * time.Hour)

	points, err := s.st.ForwardHourlyRange(r.Context(), hopID, from, to)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hop_id": hopID,
		"from":   from,
		"to":     to,
		"points": points,
	})
}

// --- 组装视图 ---

// forwardViews 一次把规则、节点、用量、展开结果都读出来再组装，避免 N+1。
func (s *Server) forwardViews(ctx context.Context) ([]*forwardRuleView, error) {
	rules, err := s.st.ListForwardRules(ctx)
	if err != nil {
		return nil, err
	}
	nodeList, err := s.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		return nil, err
	}
	usage, err := s.st.AllForwardUsage(ctx)
	if err != nil {
		return nil, err
	}

	nodes := make(map[int64]*store.Node, len(nodeList))
	for _, n := range nodeList {
		nodes[n.ID] = n
	}

	plan := forwardplan.Build(forwardplan.Inputs{Rules: rules, Nodes: nodes, FwdNodes: fwdNodes})
	problems := map[int64]string{}
	for _, p := range plan.Problems {
		problems[p.RuleID] = p.Reason
	}
	// 展开后的规则按 HopID 索引，用来显示每一跳实际转去哪。
	targets := map[int64]string{}
	for _, rules := range plan.ByNode {
		for _, fr := range rules {
			t := fr.DestHost
			if t == "" {
				t = fr.DestIP
			}
			targets[fr.HopID] = net.JoinHostPort(t, fmt.Sprintf("%d", fr.DestPort))
		}
	}

	out := make([]*forwardRuleView, 0, len(rules))
	for _, rule := range rules {
		v := &forwardRuleView{
			ForwardRule: rule,
			Entries:     forwardplan.Entries(rule, nodes, fwdNodes),
			ProtoLabel:  forwardProtoLabel(rule.Proto),
			Problem:     problems[rule.ID],
		}
		for _, h := range rule.Hops {
			hv := forwardHopView{
				ForwardHop:      h,
				ModeLabel:       forwardModeLabel(h.Mode),
				NodeOnline:      nodes[h.NodeID] != nil && nodes[h.NodeID].Status == store.StatusOnline,
				NodeCommandable: s.hub.Online(h.NodeID),
				Target:          targets[h.ID],
			}
			if u := usage[h.ID]; u != nil {
				hv.UpBytes, hv.DownBytes = u.UpBytes, u.DownBytes
				if n := nodes[h.NodeID]; n != nil {
					hv.NodeShare = quota.ForwardShare(u.UpBytes, u.DownBytes, n.BillingMode, n.TrafficRatio)
				}
			}
			v.TotalUp += hv.UpBytes
			v.TotalDown += hv.DownBytes
			v.HopViews = append(v.HopViews, hv)
		}
		out = append(out, v)
	}
	return out, nil
}

// forwardShareByNode 汇总每个节点上所有转发跳实际消耗掉的机器流量。
// 节点详情页用它把「本周期用量」拆成「转发消耗的」和「机器自己用掉的」。
func (s *Server) forwardShareByNode(ctx context.Context, nodes map[int64]*store.Node) (map[int64]int64, error) {
	usage, err := s.st.AllForwardUsage(ctx)
	if err != nil {
		return nil, err
	}
	if len(usage) == 0 {
		return map[int64]int64{}, nil
	}
	rules, err := s.st.ListForwardRules(ctx)
	if err != nil {
		return nil, err
	}

	out := map[int64]int64{}
	for _, rule := range rules {
		for _, h := range rule.Hops {
			u := usage[h.ID]
			n := nodes[h.NodeID]
			if u == nil || n == nil {
				continue
			}
			out[h.NodeID] += quota.ForwardShare(u.UpBytes, u.DownBytes, n.BillingMode, n.TrafficRatio)
		}
	}
	return out, nil
}

// --- 小工具 ---

// checkForwardNodes 提前确认选中的节点存在且没关掉转发，
// 这样报出来的是 400「节点不存在」而不是数据库外键的 500。
func (s *Server) checkForwardNodes(ctx context.Context, hops []forwardHopRequest) error {
	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		return err
	}
	for i, h := range hops {
		for _, id := range h.NodeIDs {
			n, err := s.st.GetNode(ctx, id)
			if err != nil {
				return fmt.Errorf("第 %d 跳选择的节点不存在", i+1)
			}
			if !n.Enabled {
				return fmt.Errorf("第 %d 跳选择的节点「%s」已被禁用", i+1, n.Name)
			}
			if fn := fwdNodes[id]; fn != nil && !fn.Enabled {
				return fmt.Errorf("节点「%s」关闭了端口转发，请先在转发设置里打开", n.Name)
			}
		}
	}
	return nil
}

// syncForwardAsync 在后台下发规则。
//
// 必须异步：下发要等每个探针的回执，最长几十秒，同步执行会把
// 用户的保存请求卡在那里。用 WithoutCancel 脱离 HTTP 请求的生命周期，
// 否则浏览器一关下发就断了。
func (s *Server) syncForwardAsync(ctx context.Context) {
	bg := context.WithoutCancel(ctx)
	go func() {
		syncCtx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		s.SyncForward(syncCtx, false)
	}()
}

// writeForwardStoreErr 把数据库的唯一约束冲突翻译成人话。
// 直接把 "UNIQUE constraint failed: forward_hops.node_id" 甩给用户毫无帮助。
func writeForwardStoreErr(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") &&
		strings.Contains(err.Error(), "forward_hops") {
		writeError(w, http.StatusBadRequest,
			"该节点上这个端口已经被另一条转发规则占用了，请换一个端口")
		return
	}
	handleStoreErr(w, err)
}
