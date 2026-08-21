package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/forwardplan"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// forwardSyncInterval 是转发规则对账周期。
// 主要是给"下发失败过的节点"兜底重试；正常路径上改完规则会立刻下发一次。
const forwardSyncInterval = 60 * time.Second

// forwardSyncer 记住每个节点最近一次成功下发的版本号，
// 内容没变就不重复下发 —— 每次 apply 都会让探针那边的 nft 计数归零，
// 虽然逻辑累计量能抹平，但白白折腾没有意义。
//
// 只存在内存里：面板重启后重新下发一遍是安全的（全量覆盖语义）。
type forwardSyncer struct {
	mu       sync.Mutex
	lastRev  map[int64]string
	problems []forwardplan.Problem
}

func newForwardSyncer() *forwardSyncer {
	return &forwardSyncer{lastRev: map[int64]string{}}
}

func (f *forwardSyncer) shouldPush(nodeID int64, rev string, force bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return force || f.lastRev[nodeID] != rev
}

func (f *forwardSyncer) markPushed(nodeID int64, rev string) {
	f.mu.Lock()
	f.lastRev[nodeID] = rev
	f.mu.Unlock()
}

// markFailed 清掉记住的版本号，下一轮对账一定会重试。
func (f *forwardSyncer) markFailed(nodeID int64) {
	f.mu.Lock()
	delete(f.lastRev, nodeID)
	f.mu.Unlock()
}

func (f *forwardSyncer) setProblems(p []forwardplan.Problem) {
	f.mu.Lock()
	f.problems = p
	f.mu.Unlock()
}

func (f *forwardSyncer) Problems() []forwardplan.Problem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardplan.Problem(nil), f.problems...)
}

// buildForwardPlan 从库里读出全部规则并展开成每个节点的规则集。
func (s *Server) buildForwardPlan(ctx context.Context) (forwardplan.Plan, error) {
	rules, err := s.st.ListForwardRules(ctx)
	if err != nil {
		return forwardplan.Plan{}, fmt.Errorf("读取转发规则: %w", err)
	}
	nodeList, err := s.st.ListNodes(ctx)
	if err != nil {
		return forwardplan.Plan{}, fmt.Errorf("读取节点: %w", err)
	}
	fwdNodes, err := s.st.AllForwardNodes(ctx)
	if err != nil {
		return forwardplan.Plan{}, fmt.Errorf("读取节点转发配置: %w", err)
	}

	nodes := make(map[int64]*store.Node, len(nodeList))
	for _, n := range nodeList {
		nodes[n.ID] = n
	}
	return forwardplan.Build(forwardplan.Inputs{Rules: rules, Nodes: nodes, FwdNodes: fwdNodes}), nil
}

// SyncForward 把当前的转发规则下发到所有相关节点。
//
// force 为真时无视"内容没变"的优化强制重发，用于面板上的「重新下发」按钮 ——
// 用户点它通常正是因为怀疑机器上的状态和面板对不上了。
//
// 单个节点失败不影响其他节点：离线的机器等它连回来时会自动补发。
func (s *Server) SyncForward(ctx context.Context, force bool) {
	plan, err := s.buildForwardPlan(ctx)
	if err != nil {
		s.log.Error("生成转发计划失败", "err", err)
		return
	}
	s.fwdSync.setProblems(plan.Problems)

	for _, p := range plan.Problems {
		s.log.Warn("转发规则无法下发", "规则", p.RuleName, "原因", p.Reason)
	}

	for nodeID, rules := range plan.ByNode {
		s.syncForwardNode(ctx, nodeID, rules, force)
	}
}

// syncForwardNode 给单个节点下发规则集。
func (s *Server) syncForwardNode(ctx context.Context, nodeID int64, rules []forward.Rule, force bool) {
	rev := revisionOf(rules)
	if !s.fwdSync.shouldPush(nodeID, rev, force) {
		return
	}
	if !s.hub.Online(nodeID) {
		// 离线不算失败：探针连回来时 pushRulesetOnConnect 会补上。
		// 但要清掉记住的版本号，否则它带着旧规则回来时会被误判成"已是最新"。
		s.fwdSync.markFailed(nodeID)
		return
	}

	ack, err := s.hub.SendRuleset(ctx, nodeID, protocol.ApplyRuleset{Rev: rev, Rules: rules})
	if err != nil {
		s.fwdSync.markFailed(nodeID)
		if !errors.Is(err, ErrAgentOffline) {
			s.log.Warn("下发转发规则失败", "node_id", nodeID, "err", err)
			s.addForwardEvent(ctx, nodeID, store.LevelError, fmt.Sprintf("转发规则下发失败：%v", err))
		}
		return
	}
	if !ack.OK {
		s.fwdSync.markFailed(nodeID)
		s.log.Warn("探针拒绝了转发规则", "node_id", nodeID, "err", ack.Error)
		s.addForwardEvent(ctx, nodeID, store.LevelError, fmt.Sprintf("探针拒绝转发规则：%s", ack.Error))
		return
	}

	s.fwdSync.markPushed(nodeID, rev)
	if ack.Warning != "" {
		s.log.Warn("转发规则已下发但有隐患", "node_id", nodeID, "提示", ack.Warning)
		s.addForwardEvent(ctx, nodeID, store.LevelWarn, "转发规则已生效，但："+ack.Warning)
		return
	}
	s.log.Info("转发规则已下发", "node_id", nodeID, "条数", len(rules), "版本", rev)
}

// pushRulesetOnConnect 在探针连上来时补一次下发。
//
// 必须强制下发：探针可能刚重装过、状态文件丢了，也可能在离线期间
// 面板改过规则。面板这边记的版本号说明不了机器上的实际状态。
func (s *Server) pushRulesetOnConnect(ctx context.Context, nodeID int64, nodeName string) {
	// 给探针一点时间跑完启动流程（恢复上次规则、起 DDNS 循环），
	// 免得两边同时在改数据面。
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return
	}

	plan, err := s.buildForwardPlan(ctx)
	if err != nil {
		s.log.Error("探针连接后生成转发计划失败", "node", nodeName, "err", err)
		return
	}
	rules, involved := plan.ByNode[nodeID]
	if !involved {
		// 这个节点没参与任何转发。仍然下发一个空集合，
		// 把它上面可能残留的旧规则撤干净。
		rules = []forward.Rule{}
	}
	s.syncForwardNode(ctx, nodeID, rules, true)
}

// RunForwardSync 是转发规则的后台对账循环。
//
// 正常路径上改完规则会立刻下发，这个循环只是兜底：把之前下发失败、
// 或者当时不在线的节点补上。
func (s *Server) RunForwardSync(ctx context.Context) {
	t := time.NewTicker(forwardSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncForward(ctx, false)
		}
	}
}

func (s *Server) addForwardEvent(ctx context.Context, nodeID int64, level, msg string) {
	id := nodeID
	if err := s.st.AddEvent(ctx, &id, store.EventForwardApply, level, msg); err != nil {
		s.log.Debug("写转发事件失败", "err", err)
	}
}

// revisionOf 用规则集内容算一个版本号。
//
// 内容相同就一定得到相同的版本号，面板才能靠它判断"要不要重新下发"。
// 用 sha256 而不是自增计数：面板重启后自增会从头开始，反而会把
// 本来一致的状态误判成需要重发。
func revisionOf(rules []forward.Rule) string {
	b, err := json.Marshal(rules)
	if err != nil {
		// forward.Rule 全是基本类型，序列化不可能失败。
		// 真出了意外就退回一个必然不同的值，逼迫重新下发。
		return "err-" + time.Now().UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
