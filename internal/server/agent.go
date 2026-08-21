package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/zxcll/vps-panel/internal/ingest"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

// authAgent 用节点密钥鉴权探针请求。密钥可以放在请求头或查询参数里
// （WebSocket 客户端不总是方便设置请求头）。
func (s *Server) authAgent(r *http.Request) (*store.Node, error) {
	secret := r.Header.Get(protocol.HeaderSecret)
	if secret == "" {
		secret = r.URL.Query().Get("secret")
	}
	if secret == "" {
		return nil, errors.New("缺少节点密钥")
	}

	n, err := s.st.GetNodeBySecret(r.Context(), strings.TrimSpace(secret))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errors.New("节点密钥无效")
		}
		return nil, err
	}
	if !n.Enabled {
		return nil, errors.New("该节点已被禁用")
	}
	return n, nil
}

// applyReport 是两条上报通道（WebSocket / HTTP）共用的处理逻辑。
func (s *Server) applyReport(ctx context.Context, node *store.Node, rep protocol.Report) (*protocol.AckPayload, error) {
	res, err := s.ing.Apply(ctx, node, rep)
	if err != nil {
		return nil, err
	}

	s.hub.UpdateLive(node.ID, rep)

	// 重启、换网卡这类非常规情况写进事件日志。
	// 用户看到"重启后账本继续累加"才会信任这套统计。
	switch res.Delta.Reason {
	case ingest.ReasonReboot:
		id := node.ID
		s.st.AddEvent(ctx, &id, store.EventAgentReport, store.LevelInfo,
			fmt.Sprintf("节点「%s」检测到机器重启，已把重启后的 %s 流量计入本周期账本",
				node.Name, humanBytes(res.Delta.Rx+res.Delta.Tx)))
	case ingest.ReasonCounterReset:
		id := node.ID
		s.st.AddEvent(ctx, &id, store.EventAgentReport, store.LevelWarn,
			fmt.Sprintf("节点「%s」网卡计数器发生回退（网卡可能被重建），已按重置处理", node.Name))
	case ingest.ReasonIfaceChanged:
		id := node.ID
		s.st.AddEvent(ctx, &id, store.EventAgentReport, store.LevelWarn,
			fmt.Sprintf("节点「%s」统计网卡变更为 %s，已重新建立计数基线", node.Name, rep.Iface))
	}

	// 上报后立刻判一次配额，流量跑满能在一个上报周期内被处理，
	// 不用等到下一轮定时调度。
	//
	// 放到后台跑：超额动作可能要建 SSH 连接、调 DNS 接口，最长能占几十秒。
	// 同步执行会把这个探针的 WebSocket 读循环堵住，心跳和后续指令都收不到。
	// engine 内部有按节点的互斥，重复触发不会重复关机。
	fresh, err := s.st.GetNode(ctx, node.ID)
	if err == nil {
		node = fresh
		bg := context.WithoutCancel(ctx)
		go s.eng.CheckQuota(bg, fresh, res.Usage)
	}

	st := quota.EvaluateNode(node, res.Usage)
	return &protocol.AckPayload{
		BilledBytes: st.Billed,
		QuotaBytes:  node.QuotaBytes,
		Status:      node.Status,
	}, nil
}

// handleAgentReport 是 HTTP 上报通道（WebSocket 不可用时的降级路径）。
func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	node, err := s.authAgent(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var rep protocol.Report
	if !decodeJSON(w, r, &rep) {
		return
	}
	if rep.BootID == "" {
		writeError(w, http.StatusBadRequest, "上报缺少 boot_id")
		return
	}

	ack, err := s.applyReport(r.Context(), node, rep)
	if err != nil {
		s.log.Error("处理上报失败", "node", node.Name, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// handleAgentWS 是探针的 WebSocket 长连接入口。
//
// 探针主动连面板，好处是节点不用开放任何端口，NAT 后面也能用，
// 而且面板能立刻下发关机指令。
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	node, err := s.authAgent(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 探针是我们自己的程序，不是浏览器，没有同源概念
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("WebSocket 握手失败", "node", node.Name, "err", err)
		return
	}
	// 上报体很小，限制一下防止异常客户端喂大包
	ws.SetReadLimit(64 * 1024)

	conn := &agentConn{
		nodeID:       node.ID,
		ws:           ws,
		log:          s.log,
		send:         make(chan protocol.Frame, 16),
		pending:      map[string]chan *protocol.CommandResult{},
		pendingApply: map[string]chan *protocol.ApplyAck{},
		done:         make(chan struct{}),
	}

	s.hub.register(conn)
	s.log.Info("探针已连接", "node", node.Name, "remote", clientIP(r, s.trustProxy))

	// 连接生命周期独立于 HTTP 请求：请求 ctx 在 handler 返回时就没了
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	go conn.writeLoop(ctx)

	// 探针一连上就把它该有的转发规则推一遍。
	// 探针重装、状态文件丢失、面板在它离线期间改过规则，都靠这一下补齐。
	go s.pushRulesetOnConnect(ctx, node.ID, node.Name)

	defer func() {
		s.hub.unregister(conn)
		conn.close()
		s.log.Info("探针连接已断开", "node", node.Name)
	}()

	for {
		select {
		case <-conn.done:
			return
		default:
		}

		typ, data, err := ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
				s.log.Debug("读取探针数据失败", "node", node.Name, "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			s.log.Warn("探针发来的数据无法解析", "node", node.Name, "err", err)
			continue
		}

		switch f.Type {
		case protocol.FrameReport:
			if f.Report == nil || f.Report.BootID == "" {
				continue
			}
			// 每次上报都重新读节点：用户可能刚在面板上改了配额或计费口径
			fresh, err := s.st.GetNode(ctx, node.ID)
			if err != nil {
				s.log.Warn("读取节点失败", "node_id", node.ID, "err", err)
				continue
			}
			ack, err := s.applyReport(ctx, fresh, *f.Report)
			if err != nil {
				s.log.Error("处理上报失败", "node", fresh.Name, "err", err)
				continue
			}
			payload, _ := json.Marshal(ack)
			conn.trySend(protocol.Frame{Type: protocol.FrameAck, Message: string(payload)})

		case protocol.FrameResult:
			if f.Result != nil {
				conn.deliver(f.Result)
			}

		case protocol.FrameApplyAck:
			if f.ApplyAck != nil {
				conn.deliverApply(f.ApplyAck)
			}

		case protocol.FramePong:
			if err := s.ing.TouchOnly(ctx, node.ID); err != nil {
				s.log.Debug("更新心跳时间失败", "node_id", node.ID, "err", err)
			}
		}
	}
}

// handleNodeInstallInfo 返回某个节点的一键安装命令，前端直接展示给用户复制。
func (s *Server) handleNodeInstallInfo(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}

	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":       n.Secret,
		"server":       wsURL(base),
		"install_cmd":  fmt.Sprintf("curl -fsSL %s/agent/install.sh?secret=%s | sudo bash", base, n.Secret),
		"manual_cmd":   fmt.Sprintf("vps-agent --server %s --secret %s", wsURL(base), n.Secret),
		"uninstall":    "sudo systemctl disable --now vps-agent && sudo rm -f /usr/local/bin/vps-agent /etc/systemd/system/vps-agent.service",
		"base_url":     base,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// baseURL 推断探针该用哪个地址连面板。
// 配置里写死的优先；没配就按当前请求猜，方便本机试跑。
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func wsURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}

// humanBytes 与 engine 包里的实现一致，这里为了避免包间循环依赖单独放一份。
func humanBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
