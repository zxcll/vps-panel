package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/billing"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

// nodeView 是返回给前端的节点视图：库里的字段 + 算出来的用量和实时状态。
type nodeView struct {
	*store.Node
	Usage         *store.Usage `json:"usage"`
	QuotaStatus   quota.Status `json:"quota_status"`
	Live          *Live        `json:"live"`
	CycleProgress float64      `json:"cycle_progress"`
	AgentOnline   bool         `json:"agent_online"`
	HasSSHSecret  bool         `json:"has_ssh_secret"`
	// BillingModeLabel 等中文标签由后端给，前端不用维护第二份枚举映射。
	BillingModeLabel string `json:"billing_mode_label"`
	ActionLabel      string `json:"action_label"`

	// ForwardShare 是本节点这一周期的用量里，有多少是端口转发消耗掉的。
	//
	// 它是从转发计数**换算**出来的估算值，不是从账本里拆出来的：
	// QuotaStatus.Billed 永远以网卡计数器为准（转发流量本来就已经算在里面了），
	// 这个字段只是帮用户解释「为什么规则加起来才 50G，机器却用了 100G」。
	// 换算口径见 quota.ForwardShare。
	ForwardShare int64 `json:"forward_share"`
}

func (s *Server) viewNode(n *store.Node, u *store.Usage, live *Live) *nodeView {
	v := &nodeView{
		Node:             n,
		Usage:            u,
		QuotaStatus:      quota.EvaluateNode(n, u),
		Live:             live,
		AgentOnline:      s.hub.Online(n.ID),
		HasSSHSecret:     len(n.SSHSecretEnc) > 0,
		BillingModeLabel: billingModeLabel(n.BillingMode),
		ActionLabel:      exceedActionLabel(n.ActionOnExceed),
	}
	v.CycleProgress = billing.Progress(time.Now().UTC(), n.CycleStart, n.CycleEnd)
	return v
}

func billingModeLabel(mode string) string {
	switch mode {
	case store.BillingMax:
		return "单向（出入站取大）"
	case store.BillingOut:
		return "仅出站"
	case store.BillingIn:
		return "仅入站"
	default:
		return "双向（出入站相加）"
	}
}

func exceedActionLabel(a string) string {
	switch a {
	case store.ActionShutdownAgent:
		return "探针本地关机"
	case store.ActionShutdownSSH:
		return "SSH 远程关机"
	case store.ActionCommand:
		return "执行自定义命令"
	case store.ActionDNSOnly:
		return "仅切换解析"
	default:
		return "不处理"
	}
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	usages, err := s.st.AllUsage(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	lives := s.hub.AllLive()

	out := make([]*nodeView, 0, len(nodes))
	byID := make(map[int64]*store.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
		u := usages[n.ID]
		if u == nil {
			u = &store.Usage{NodeID: n.ID}
		}
		out = append(out, s.viewNode(n, u, lives[n.ID]))
	}
	s.attachForwardShare(ctx, byID, out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	u, err := s.st.GetUsage(ctx, n.ID)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	v := s.viewNode(n, u, s.hub.LiveOf(n.ID))
	s.attachForwardShare(ctx, map[int64]*store.Node{n.ID: n}, []*nodeView{v})
	writeJSON(w, http.StatusOK, v)
}

// attachForwardShare 给节点视图补上「其中多少来自转发」。
//
// 查询失败只记日志不报错：这只是个辅助说明，不该让整个节点列表打不开。
func (s *Server) attachForwardShare(ctx context.Context, nodes map[int64]*store.Node, views []*nodeView) {
	share, err := s.forwardShareByNode(ctx, nodes)
	if err != nil {
		s.log.Debug("统计转发流量占用失败", "err", err)
		return
	}
	for _, v := range views {
		v.ForwardShare = share[v.ID]
	}
}

// nodeRequest 是新建/编辑节点的请求体。
//
// 敏感字段用指针：nil 表示"不改"，空串表示"清空"。
// 这样编辑节点时不必把 SSH 密码重新填一遍。
type nodeRequest struct {
	Name   string `json:"name"`
	Remark string `json:"remark"`
	IPv4   string `json:"ipv4"`
	IPv6   string `json:"ipv6"`

	QuotaBytes   int64   `json:"quota_bytes"`
	BillingMode  string  `json:"billing_mode"`
	TrafficRatio float64 `json:"traffic_ratio"`
	WarnPercent  int     `json:"warn_percent"`

	ResetDay int    `json:"reset_day"`
	ResetTZ  string `json:"reset_tz"`

	ActionOnExceed     string `json:"action_on_exceed"`
	ExceedCommand      string `json:"exceed_command"`
	AutoRecoverOnReset bool   `json:"auto_recover_on_reset"`

	SSHHost    string  `json:"ssh_host"`
	SSHPort    int     `json:"ssh_port"`
	SSHUser    string  `json:"ssh_user"`
	SSHAuth    string  `json:"ssh_auth"`
	SSHSecret  *string `json:"ssh_secret"`   // 密码或私钥 PEM
	SSHKeyPass *string `json:"ssh_key_pass"` // 私钥口令
	SSHHostKey *string `json:"ssh_host_key"` // 传空串可清掉，触发重新 TOFU
	SSHUseSudo bool    `json:"ssh_use_sudo"`

	ProbePort int  `json:"probe_port"`
	Enabled   bool `json:"enabled"`
}

// validate 校验并规整请求，返回给用户看的错误。
func (req *nodeRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fmt.Errorf("节点名称不能为空")
	}

	req.IPv4 = strings.TrimSpace(req.IPv4)
	if req.IPv4 != "" {
		ip := net.ParseIP(req.IPv4)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("IPv4 地址 %q 格式不正确", req.IPv4)
		}
	}
	req.IPv6 = strings.TrimSpace(req.IPv6)
	if req.IPv6 != "" {
		ip := net.ParseIP(req.IPv6)
		if ip == nil || ip.To4() != nil {
			return fmt.Errorf("IPv6 地址 %q 格式不正确", req.IPv6)
		}
	}

	if req.BillingMode == "" {
		req.BillingMode = store.BillingSum
	}
	if !quota.ValidMode(req.BillingMode) {
		return fmt.Errorf("未知的计费口径 %q", req.BillingMode)
	}

	if req.TrafficRatio <= 0 {
		req.TrafficRatio = 1
	}
	if req.TrafficRatio > 100 {
		return fmt.Errorf("计费倍率 %.2f 不合理，请填 0.1 ~ 10 之间的值", req.TrafficRatio)
	}

	if req.QuotaBytes < 0 {
		return fmt.Errorf("流量配额不能为负数")
	}

	if req.WarnPercent < 0 || req.WarnPercent > 100 {
		return fmt.Errorf("预警百分比需要在 0 ~ 100 之间")
	}

	if req.ResetDay == 0 {
		req.ResetDay = 1
	}
	if req.ResetDay < 1 || req.ResetDay > 31 {
		return fmt.Errorf("重置日需要在 1 ~ 31 之间")
	}

	if req.ResetTZ == "" {
		req.ResetTZ = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(req.ResetTZ); err != nil {
		return fmt.Errorf("无法识别时区 %q", req.ResetTZ)
	}

	if req.ActionOnExceed == "" {
		req.ActionOnExceed = store.ActionNone
	}
	if !action.ValidAction(req.ActionOnExceed) {
		return fmt.Errorf("未知的超额动作 %q", req.ActionOnExceed)
	}
	req.ExceedCommand = strings.TrimSpace(req.ExceedCommand)
	if req.ActionOnExceed == store.ActionCommand && req.ExceedCommand == "" {
		return fmt.Errorf("选择了「执行自定义命令」，请填写要执行的命令")
	}

	req.SSHHost = strings.TrimSpace(req.SSHHost)
	if req.SSHPort == 0 {
		req.SSHPort = 22
	}
	if req.SSHPort < 1 || req.SSHPort > 65535 {
		return fmt.Errorf("SSH 端口 %d 不合法", req.SSHPort)
	}
	if req.SSHUser == "" {
		req.SSHUser = "root"
	}
	if req.SSHAuth == "" {
		req.SSHAuth = "password"
	}
	if req.SSHAuth != "password" && req.SSHAuth != "key" {
		return fmt.Errorf("SSH 认证方式只能是 password 或 key")
	}

	if req.ProbePort < 0 || req.ProbePort > 65535 {
		return fmt.Errorf("探测端口 %d 不合法", req.ProbePort)
	}

	// SSH 关机需要能连上机器，配置不全时提前拦下来，
	// 免得等到流量跑满那一刻才发现关不掉
	if req.ActionOnExceed == store.ActionShutdownSSH && req.SSHHost == "" {
		return fmt.Errorf("超额动作选了「SSH 远程关机」，必须填写 SSH 地址")
	}

	return nil
}

// applyTo 把请求写进节点结构。
func (s *Server) applyTo(n *store.Node, req *nodeRequest) error {
	n.Name = req.Name
	n.Remark = req.Remark
	n.IPv4 = req.IPv4
	n.IPv6 = req.IPv6
	n.QuotaBytes = req.QuotaBytes
	n.BillingMode = req.BillingMode
	n.TrafficRatio = req.TrafficRatio
	n.WarnPercent = req.WarnPercent
	n.ResetDay = req.ResetDay
	n.ResetTZ = req.ResetTZ
	n.ActionOnExceed = req.ActionOnExceed
	n.ExceedCommand = req.ExceedCommand
	n.AutoRecoverOnReset = req.AutoRecoverOnReset
	n.SSHHost = req.SSHHost
	n.SSHPort = req.SSHPort
	n.SSHUser = req.SSHUser
	n.SSHAuth = req.SSHAuth
	n.SSHUseSudo = req.SSHUseSudo
	n.ProbePort = req.ProbePort
	n.Enabled = req.Enabled

	if req.SSHSecret != nil {
		enc, err := s.cipher.Encrypt(strings.TrimSpace(*req.SSHSecret))
		if err != nil {
			return fmt.Errorf("加密 SSH 凭据: %w", err)
		}
		n.SSHSecretEnc = enc
	}
	if req.SSHKeyPass != nil {
		enc, err := s.cipher.Encrypt(*req.SSHKeyPass)
		if err != nil {
			return fmt.Errorf("加密私钥口令: %w", err)
		}
		n.SSHKeyPassEnc = enc
	}
	if req.SSHHostKey != nil {
		n.SSHHostKey = strings.TrimSpace(*req.SSHHostKey)
	}
	return nil
}

func newSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成节点密钥: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req nodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	secret, err := newSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	n := &store.Node{Secret: secret, Status: store.StatusUnknown}
	if err := s.applyTo(n, &req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 建节点时就把账单周期算好，省得等第一次调度
	loc, _ := billing.LoadLocation(n.ResetTZ)
	n.CycleStart, n.CycleEnd = billing.CurrentCycle(time.Now().UTC(), n.ResetDay, loc)

	ctx := r.Context()
	if err := s.st.CreateNode(ctx, n); err != nil {
		handleStoreErr(w, err)
		return
	}

	id := n.ID
	s.st.AddEvent(ctx, &id, store.EventSystem, store.LevelInfo,
		fmt.Sprintf("节点「%s」已创建", n.Name))

	u, _ := s.st.GetUsage(ctx, n.ID)
	writeJSON(w, http.StatusCreated, s.viewNode(n, u, nil))
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}

	var req nodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	oldDay, oldTZ := n.ResetDay, n.ResetTZ
	if err := s.applyTo(n, &req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx := r.Context()
	if err := s.st.UpdateNode(ctx, n); err != nil {
		handleStoreErr(w, err)
		return
	}

	// 改了重置日或时区就立刻重算周期边界，不用等下一轮调度
	if oldDay != n.ResetDay || oldTZ != n.ResetTZ {
		loc, _ := billing.LoadLocation(n.ResetTZ)
		start, end := billing.CurrentCycle(time.Now().UTC(), n.ResetDay, loc)
		if err := s.st.SetNodeCycle(ctx, n.ID, start, end, false); err != nil {
			s.log.Warn("重算账单周期失败", "node", n.Name, "err", err)
		} else {
			n.CycleStart, n.CycleEnd = start, end
		}
	}

	u, _ := s.st.GetUsage(ctx, n.ID)
	writeJSON(w, http.StatusOK, s.viewNode(n, u, s.hub.LiveOf(n.ID)))
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := s.st.DeleteNode(ctx, n.ID); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.hub.Forget(n.ID)
	s.st.AddEvent(ctx, nil, store.EventSystem, store.LevelInfo,
		fmt.Sprintf("节点「%s」已删除", n.Name))
	writeJSON(w, http.StatusOK, map[string]string{"message": "节点已删除"})
}

// handleResetNode 手动把流量清零并开启新周期。
func (s *Server) handleResetNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	if err := s.eng.RollCycle(r.Context(), n, "手动清零"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, _ := s.st.GetUsage(r.Context(), n.ID)
	writeJSON(w, http.StatusOK, s.viewNode(n, u, s.hub.LiveOf(n.ID)))
}

type shutdownRequest struct {
	// Via 可选 agent / ssh，留空则自动选择。
	Via string `json:"via"`
}

func (s *Server) handleShutdownNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}

	var req shutdownRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	if req.Via != "" && req.Via != action.ViaAgent && req.Via != action.ViaSSH {
		writeError(w, http.StatusBadRequest, "via 只能是 agent 或 ssh")
		return
	}

	ctx, cancel := actionContext(r.Context())
	defer cancel()

	outcome, err := s.exec.Shutdown(ctx, n, req.Via, "面板手动关机")
	id := n.ID
	if err != nil {
		msg := fmt.Sprintf("节点「%s」手动关机失败：%v", n.Name, err)
		s.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelError, msg)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelWarn,
		fmt.Sprintf("节点「%s」已手动关机（通道：%s）", n.Name, outcome.Via))
	if err := s.st.SetNodeStatus(ctx, n.ID, store.StatusStopped); err != nil {
		s.log.Warn("更新节点状态失败", "node", n.Name, "err", err)
	}

	writeJSON(w, http.StatusOK, outcome)
}

type execRequest struct {
	Script string `json:"script"`
	Via    string `json:"via"`
}

func (s *Server) handleExecNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}

	var req execRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Script) == "" {
		writeError(w, http.StatusBadRequest, "命令不能为空")
		return
	}

	ctx, cancel := actionContext(r.Context())
	defer cancel()

	outcome, err := s.exec.Exec(ctx, n, req.Script, req.Via, "面板手动执行")
	id := n.ID
	if err != nil {
		s.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelError,
			fmt.Sprintf("节点「%s」执行命令失败：%v", n.Name, err))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.st.AddEvent(ctx, &id, store.EventActionRun, store.LevelInfo,
		fmt.Sprintf("节点「%s」执行了命令：%s", n.Name, req.Script))
	writeJSON(w, http.StatusOK, outcome)
}

// handleTestSSH 测试 SSH 连通性，顺便把主机密钥记下来。
func (s *Server) handleTestSSH(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	if !n.HasSSH() {
		writeError(w, http.StatusBadRequest, "该节点尚未配置 SSH 地址或凭据")
		return
	}

	ctx, cancel := actionContext(r.Context())
	defer cancel()

	hostKey, err := s.exec.SSH().Probe(ctx, n)

	// 首次连接成功就把主机密钥固定下来，之后再变化会被拒绝
	saved := false
	if hostKey != "" && n.SSHHostKey == "" && err == nil {
		if e := s.st.SetNodeHostKey(ctx, n.ID, hostKey); e == nil {
			saved = true
		}
	}

	if err != nil {
		resp := map[string]any{"ok": false, "error": err.Error(), "host_key": hostKey}
		if action.IsHostKeyMismatch(err) {
			resp["host_key_mismatch"] = true
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"host_key":        hostKey,
		"host_key_saved":  saved,
		"message":         "SSH 连接正常，可以执行远程关机",
		"effective_sudo":  n.SSHUseSudo,
		"shutdown_script": action.ShutdownScript,
	})
}

// handleRotateSecret 重置节点密钥。旧探针会立刻失效，需要重装。
func (s *Server) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	secret, err := newSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx := r.Context()
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE nodes SET secret=?, updated_at=? WHERE id=?`,
		secret, time.Now().UTC().Unix(), n.ID); err != nil {
		handleStoreErr(w, err)
		return
	}

	s.hub.Forget(n.ID)
	id := n.ID
	s.st.AddEvent(ctx, &id, store.EventSystem, store.LevelWarn,
		fmt.Sprintf("节点「%s」的密钥已重置，需要用新的安装命令重装探针", n.Name))

	writeJSON(w, http.StatusOK, map[string]string{
		"secret":  secret,
		"message": "密钥已重置，请用新的安装命令重新部署探针",
	})
}
