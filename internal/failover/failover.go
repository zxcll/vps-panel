// Package failover 决定"域名此刻该解析到哪台机器"，并驱动 DNS 服务商完成切换。
//
// 触发切换的四种情形（每条记录可单独开关）：
//   - 节点流量耗尽
//   - 节点离线
//   - 节点流量到达预警线（提前切走，给自己留余量）
//   - 用户手动一键切换
//
// 两个防误切的设计：
//   - 探针失联后再做一次 TCP 拨测。探针进程挂了但机器还活着时不该切
//   - 连续失败 N 次才判定 down，且两次切换之间有冷却期，避免来回抖
package failover

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

// LiveChecker 让 failover 能问 WebSocket hub"这个探针现在连着吗"。
// 有活跃长连接是比 last_seen 更即时的在线信号。
type LiveChecker interface {
	Online(nodeID int64) bool
}

type Manager struct {
	st       *store.Store
	cipher   *crypto.Cipher
	notifier *notify.Notifier
	live     LiveChecker
	log      *slog.Logger

	mu sync.Mutex
	// failCounts 记录每个节点连续判定失败的次数，用于抑制抖动。
	failCounts map[int64]int
	// providers 缓存已构造的服务商客户端（Cloudflare 会缓存 zone ID）。
	// key 是 providerID，value 里带上凭据更新时间，凭据改了就重建。
	providers map[int64]*cachedProvider
	// lastState 记录上次判定的在线状态，用于只在状态翻转时写事件日志。
	lastAlive map[int64]bool
}

type cachedProvider struct {
	p       dns.Provider
	updated time.Time
}

func New(st *store.Store, c *crypto.Cipher, n *notify.Notifier, live LiveChecker, log *slog.Logger) *Manager {
	return &Manager{
		st: st, cipher: c, notifier: n, live: live, log: log,
		failCounts: map[int64]int{},
		providers:  map[int64]*cachedProvider{},
		lastAlive:  map[int64]bool{},
	}
}

// NodeState 是一个节点在某一刻的综合状态。
type NodeState struct {
	Node  *store.Node
	Usage *store.Usage
	Quota quota.Status
	// Alive 表示机器可用：探针在线，或者探针失联但 TCP 拨测通得过。
	Alive bool
	// AgentOnline 单指探针连接是否活跃。
	AgentOnline bool
	// Reason 说明 Alive=false 的原因。
	Reason string
}

// Snapshot 采集所有节点的当前状态。
func (m *Manager) Snapshot(ctx context.Context) (map[int64]*NodeState, error) {
	cfg, err := m.st.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := m.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	usages, err := m.st.AllUsage(ctx)
	if err != nil {
		return nil, err
	}

	offlineAfter := time.Duration(cfg.OfflineAfterSec) * time.Second
	if offlineAfter <= 0 {
		offlineAfter = 90 * time.Second
	}
	now := time.Now().UTC()

	out := make(map[int64]*NodeState, len(nodes))
	for _, n := range nodes {
		s := &NodeState{Node: n, Usage: usages[n.ID]}
		s.Quota = quota.EvaluateNode(n, s.Usage)
		s.AgentOnline = m.live != nil && m.live.Online(n.ID)

		heartbeatFresh := n.LastSeen != nil && now.Sub(*n.LastSeen) <= offlineAfter

		switch {
		case s.AgentOnline || heartbeatFresh:
			s.Alive = true
		case cfg.TCPProbeEnabled && n.ProbeHost() != "":
			// 探针失联，但机器可能只是探针进程挂了。拨一下端口确认。
			timeout := time.Duration(cfg.TCPProbeTimeoutMS) * time.Millisecond
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			if tcpReachable(ctx, n.ProbeHost(), n.EffectiveProbePort(), timeout) {
				s.Alive = true
				s.Reason = "探针失联，但端口拨测可达（探针进程可能已退出）"
			} else {
				s.Reason = "探针失联，且端口拨测不可达"
			}
		default:
			s.Reason = "探针失联"
		}

		out[n.ID] = s
	}
	return out, nil
}

// tcpReachable 尝试建立 TCP 连接。能连上（哪怕立刻被拒绝也算机器活着）就返回 true。
func tcpReachable(ctx context.Context, host string, port int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// MarkAlive 把 Alive 判定结果喂进连续失败计数器，返回"是否确认下线"。
// 需要连续失败 fail_threshold 次才算数，避免一次网络抖动就切走域名。
func (m *Manager) MarkAlive(nodeID int64, alive bool, threshold int) (confirmedDown bool) {
	if threshold <= 0 {
		threshold = 1
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if alive {
		delete(m.failCounts, nodeID)
		return false
	}
	m.failCounts[nodeID]++
	return m.failCounts[nodeID] >= threshold
}

// FailCount 返回某节点当前的连续失败次数，供前端展示。
func (m *Manager) FailCount(nodeID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failCounts[nodeID]
}

// eligible 判断一个节点能否作为某条记录的解析目标。
// 各种"不能用"的条件是按记录上的开关配置来的——用户可能只想在流量耗尽时切，
// 不想因为短暂离线就切。
func eligible(s *NodeState, rec *store.DNSRecord, confirmedDown bool) (bool, string) {
	n := s.Node

	if !n.Enabled {
		return false, "节点已禁用"
	}
	if value := recordValue(n, rec.RecordType); value == "" {
		return false, fmt.Sprintf("节点未填写 %s 记录所需的 IP 地址", rec.RecordType)
	}
	if n.Status == store.StatusStopped {
		return false, "节点已被面板关停"
	}
	if rec.SwitchOnExceed && s.Quota.Exceeded {
		return false, "流量已超额"
	}
	if rec.SwitchOnWarn && s.Quota.Warning {
		return false, fmt.Sprintf("流量已达预警线（%.1f%%）", s.Quota.Percent)
	}
	if rec.SwitchOnOffline && confirmedDown {
		return false, s.Reason
	}
	return true, ""
}

// recordValue 取节点上对应记录类型的 IP。
func recordValue(n *store.Node, rtype string) string {
	if rtype == "AAAA" {
		return n.IPv6
	}
	return n.IPv4
}

// Decision 是一次选主的结论。
type Decision struct {
	RecordID   int64  `json:"record_id"`
	RecordName string `json:"record_name"`
	// TargetNodeID 为 0 表示没有任何候选节点可用。
	TargetNodeID   int64  `json:"target_node_id"`
	TargetNodeName string `json:"target_node_name"`
	TargetValue    string `json:"target_value"`
	CurrentValue   string `json:"current_value"`
	// Change 表示解析值确实需要改动。
	Change bool `json:"change"`
	// Skipped 说明为什么没有执行切换（冷却中、无可用节点等）。
	Skipped string `json:"skipped,omitempty"`
	// Candidates 记录每个候选节点被选中/淘汰的原因，便于用户理解决策。
	Candidates []CandidateInfo `json:"candidates"`
}

type CandidateInfo struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	Priority int    `json:"priority"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

// Decide 为一条记录选主，不产生任何副作用。
func (m *Manager) Decide(rec *store.DNSRecord, states map[int64]*NodeState, cfg store.Settings) Decision {
	d := Decision{RecordID: rec.ID, RecordName: rec.Name, CurrentValue: rec.CurrentValue}

	members := append([]store.DNSMember(nil), rec.Members...)
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].Priority != members[j].Priority {
			return members[i].Priority < members[j].Priority
		}
		return members[i].NodeID < members[j].NodeID
	})

	for _, mem := range members {
		s := states[mem.NodeID]
		info := CandidateInfo{NodeID: mem.NodeID, NodeName: mem.NodeName, Priority: mem.Priority}
		if s == nil {
			info.Reason = "节点不存在"
			d.Candidates = append(d.Candidates, info)
			continue
		}
		if info.NodeName == "" {
			info.NodeName = s.Node.Name
		}

		confirmedDown := !s.Alive && m.FailCount(mem.NodeID) >= maxInt(cfg.FailThreshold, 1)
		ok, reason := eligible(s, rec, confirmedDown)
		info.Eligible = ok
		info.Reason = reason
		d.Candidates = append(d.Candidates, info)

		if ok && d.TargetNodeID == 0 {
			d.TargetNodeID = mem.NodeID
			d.TargetNodeName = info.NodeName
			d.TargetValue = recordValue(s.Node, rec.RecordType)
		}
	}

	switch {
	case d.TargetNodeID == 0:
		// 一个可用的都没有。此时保持现状——把域名指向一台确定不可用的机器
		// 没有任何好处，而乱切只会让问题更难排查。
		d.Skipped = "没有可用的候选节点，保持当前解析不变"
	case d.TargetValue != rec.CurrentValue:
		d.Change = true
	}

	return d
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
