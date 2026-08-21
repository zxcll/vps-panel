package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zxcll/vps-panel/internal/protocol"
)

// Live 是节点的实时状态，只存在内存里。
// 这些数据丢了无所谓——重连后探针会重新报，而计费账本在数据库里。
type Live struct {
	NodeID       int64     `json:"node_id"`
	Connected    bool      `json:"connected"`
	ConnectedAt  time.Time `json:"connected_at,omitempty"`
	LastReport   time.Time `json:"last_report,omitempty"`
	RxRate       float64   `json:"rx_rate"` // 字节/秒
	TxRate       float64   `json:"tx_rate"`
	Uptime       int64     `json:"uptime"`
	Load1        float64   `json:"load1"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemTotal     int64     `json:"mem_total"`
	MemUsed      int64     `json:"mem_used"`
	DiskTotal    int64     `json:"disk_total"`
	DiskUsed     int64     `json:"disk_used"`
	Iface        string    `json:"iface"`
	AgentVersion string    `json:"agent_version"`

	// 上一次上报的原始计数器，用来算瞬时速率
	lastRx, lastTx int64
	lastAt         time.Time
	lastBootID     string
}

// Hub 管理所有探针的 WebSocket 连接，并维护实时状态。
type Hub struct {
	log *slog.Logger

	mu    sync.RWMutex
	conns map[int64]*agentConn
	live  map[int64]*Live
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		log:   log,
		conns: map[int64]*agentConn{},
		live:  map[int64]*Live{},
	}
}

type agentConn struct {
	nodeID int64
	ws     *websocket.Conn
	log    *slog.Logger

	// send 是发往探针的出站队列。单独一个 goroutine 消费，
	// 避免多处并发写同一个 WebSocket（websocket 不允许并发写）。
	send chan protocol.Frame

	mu      sync.Mutex
	pending map[string]chan *protocol.CommandResult
	// pendingApply 和 pending 分开：转发规则集用 Rev 对号，
	// 回执类型也不一样，混在一个 map 里只会逼出一堆类型断言。
	pendingApply map[string]chan *protocol.ApplyAck
	closed       bool
	done         chan struct{}
}

// register 挂上新连接。同一个节点重连时踢掉旧连接。
func (h *Hub) register(c *agentConn) {
	h.mu.Lock()
	old := h.conns[c.nodeID]
	h.conns[c.nodeID] = c

	l := h.live[c.nodeID]
	if l == nil {
		l = &Live{NodeID: c.nodeID}
		h.live[c.nodeID] = l
	}
	l.Connected = true
	l.ConnectedAt = time.Now().UTC()
	h.mu.Unlock()

	if old != nil {
		h.log.Info("探针重复连接，断开旧连接", "node_id", c.nodeID)
		old.close()
	}
}

func (h *Hub) unregister(c *agentConn) {
	h.mu.Lock()
	// 只有当前登记的还是自己时才摘除，避免踢掉刚建立的新连接
	if h.conns[c.nodeID] == c {
		delete(h.conns, c.nodeID)
		if l := h.live[c.nodeID]; l != nil {
			l.Connected = false
		}
	}
	h.mu.Unlock()
}

// Online 判断探针是否有活跃的 WebSocket 连接。
func (h *Hub) Online(nodeID int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

// Live 返回某节点的实时状态副本。
func (h *Hub) LiveOf(nodeID int64) *Live {
	h.mu.RLock()
	defer h.mu.RUnlock()
	l := h.live[nodeID]
	if l == nil {
		return nil
	}
	cp := *l
	return &cp
}

// AllLive 返回所有节点的实时状态副本，供总览页一次取回。
func (h *Hub) AllLive() map[int64]*Live {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[int64]*Live, len(h.live))
	for id, l := range h.live {
		cp := *l
		out[id] = &cp
	}
	return out
}

// Forget 在节点被删除时清掉它的实时状态。
func (h *Hub) Forget(nodeID int64) {
	h.mu.Lock()
	if c, ok := h.conns[nodeID]; ok {
		delete(h.conns, nodeID)
		go c.close()
	}
	delete(h.live, nodeID)
	h.mu.Unlock()
}

// UpdateLive 用一次上报刷新实时状态并计算瞬时速率。
// HTTP 上报通道也会调用它，所以速率计算不依赖 WebSocket。
func (h *Hub) UpdateLive(nodeID int64, r protocol.Report) {
	now := time.Now().UTC()

	h.mu.Lock()
	defer h.mu.Unlock()

	l := h.live[nodeID]
	if l == nil {
		l = &Live{NodeID: nodeID}
		h.live[nodeID] = l
	}

	// 速率只在"同一次开机、计数器单调递增"时才有意义。
	// 重启或计数器回退时跳过这一轮，否则会算出一个巨大的假峰值。
	if !l.lastAt.IsZero() && l.lastBootID == r.BootID && r.Rx >= l.lastRx && r.Tx >= l.lastTx {
		if dt := now.Sub(l.lastAt).Seconds(); dt > 0.5 {
			l.RxRate = float64(r.Rx-l.lastRx) / dt
			l.TxRate = float64(r.Tx-l.lastTx) / dt
		}
	} else {
		l.RxRate, l.TxRate = 0, 0
	}

	l.lastRx, l.lastTx, l.lastAt, l.lastBootID = r.Rx, r.Tx, now, r.BootID
	l.LastReport = now
	l.Uptime = r.Uptime
	l.Load1 = r.Load1
	l.CPUPercent = r.CPUPercent
	l.MemTotal, l.MemUsed = r.MemTotal, r.MemUsed
	l.DiskTotal, l.DiskUsed = r.DiskTotal, r.DiskUsed
	l.Iface = r.Iface
	l.AgentVersion = r.AgentVersion
}

// ErrAgentOffline 表示探针当前没有连接。
var ErrAgentOffline = errors.New("探针未连接")

// Send 下发一条指令并等待探针回执。
func (h *Hub) Send(ctx context.Context, nodeID int64, cmd protocol.Command) (*protocol.CommandResult, error) {
	h.mu.RLock()
	c := h.conns[nodeID]
	h.mu.RUnlock()

	if c == nil {
		return nil, ErrAgentOffline
	}
	if cmd.ID == "" {
		return nil, fmt.Errorf("指令缺少 ID")
	}

	ch := make(chan *protocol.CommandResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrAgentOffline
	}
	c.pending[cmd.ID] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
	}()

	timeout := time.Duration(cmd.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// 留出网络往返的余量，别和探针侧的执行超时卡在同一刻
	waitFor := timeout + 10*time.Second

	select {
	case c.send <- protocol.Frame{Type: protocol.FrameCommand, Command: &cmd}:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("下发指令超时：探针出站队列已满")
	case <-c.done:
		return nil, ErrAgentOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case res := <-ch:
		return res, nil
	case <-time.After(waitFor):
		// 关机指令常常等不到回执——机器已经断电了。这不算失败，
		// 上层会结合"探针随后离线"来判断实际效果。
		if cmd.Cmd == protocol.CmdShutdown {
			return &protocol.CommandResult{
				ID: cmd.ID, OK: true,
				Output: "指令已下发，但未收到回执（关机时连接被切断属于正常现象）",
			}, nil
		}
		return nil, fmt.Errorf("等待探针回执超时（%s）", waitFor)
	case <-c.done:
		if cmd.Cmd == protocol.CmdShutdown {
			return &protocol.CommandResult{
				ID: cmd.ID, OK: true,
				Output: "指令已下发，探针连接随即断开（关机生效的典型表现）",
			}, nil
		}
		return nil, ErrAgentOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// applyTimeout 是等转发规则回执的上限。
// 探针那边要拨 DNS、调 nft、绑端口，比执行一条命令慢，但也不该超过这个量级。
const applyTimeout = 30 * time.Second

// SendRuleset 给某个节点下发一份完整的转发规则集并等回执。
//
// 全量覆盖语义：探针收到后让本机状态与之完全一致。所以「撤掉某节点上的所有规则」
// 就是给它下发一个空集合，不需要单独的删除指令。
func (h *Hub) SendRuleset(ctx context.Context, nodeID int64, rs protocol.ApplyRuleset) (*protocol.ApplyAck, error) {
	h.mu.RLock()
	c := h.conns[nodeID]
	h.mu.RUnlock()

	if c == nil {
		return nil, ErrAgentOffline
	}
	if rs.Rev == "" {
		return nil, fmt.Errorf("规则集缺少版本号")
	}

	ch := make(chan *protocol.ApplyAck, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrAgentOffline
	}
	c.pendingApply[rs.Rev] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pendingApply, rs.Rev)
		c.mu.Unlock()
	}()

	select {
	case c.send <- protocol.Frame{Type: protocol.FrameApplyRuleset, ApplyRuleset: &rs}:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("下发转发规则超时：探针出站队列已满")
	case <-c.done:
		return nil, ErrAgentOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case ack := <-ch:
		return ack, nil
	case <-time.After(applyTimeout):
		return nil, fmt.Errorf("等待探针回执超时（%s）", applyTimeout)
	case <-c.done:
		return nil, ErrAgentOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliverApply 把转发规则回执交给等待中的调用方。
func (c *agentConn) deliverApply(ack *protocol.ApplyAck) {
	c.mu.Lock()
	ch := c.pendingApply[ack.Rev]
	c.mu.Unlock()

	if ch == nil {
		// 面板侧已经等超时了，或者探针把旧版本的回执补发了过来。
		// 记一笔就够，不用当错误处理。
		c.log.Debug("收到无人等待的转发规则回执", "node_id", c.nodeID, "rev", ack.Rev)
		return
	}
	select {
	case ch <- ack:
	default:
	}
}

// deliver 把探针回执交给等待中的调用方。
func (c *agentConn) deliver(res *protocol.CommandResult) {
	c.mu.Lock()
	ch := c.pending[res.ID]
	c.mu.Unlock()

	if ch == nil {
		c.log.Warn("收到无人等待的指令回执", "cmd_id", res.ID)
		return
	}
	select {
	case ch <- res:
	default:
	}
}

func (c *agentConn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	close(c.done)
	c.ws.Close(websocket.StatusNormalClosure, "")
}

// writeLoop 是唯一向 WebSocket 写数据的地方。
func (c *agentConn) writeLoop(ctx context.Context) {
	// 定期发 ping：探针在 NAT 后面时，中间设备可能悄悄掐掉空闲连接
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return

		case f := <-c.send:
			if err := c.writeFrame(ctx, f); err != nil {
				c.log.Debug("发送数据帧失败", "node_id", c.nodeID, "err", err)
				c.close()
				return
			}

		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.log.Debug("心跳 ping 失败，关闭连接", "node_id", c.nodeID, "err", err)
				c.close()
				return
			}
		}
	}
}

func (c *agentConn) writeFrame(ctx context.Context, f protocol.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// trySend 尽力发送，队列满时丢弃。用于 ack 这种丢了也无所谓的帧。
func (c *agentConn) trySend(f protocol.Frame) {
	select {
	case c.send <- f:
	default:
	}
}
