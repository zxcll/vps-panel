package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/agent"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// 探针远程升级。
//
// 目的很直接：装了十几台节点之后，每次发版都要挨台 SSH 上去跑一遍安装脚本，
// 太蠢了。面板本来就存着各架构的探针二进制（一键安装就是从这儿下的），
// 也本来就有一条到每台机器的 WebSocket，把这两件事接起来就行。
//
// 流程：面板发 CmdUpgrade → 探针从 /agent/download 下新二进制 → 校验 →
// 替换自己 → 回复面板 → 退出 → systemd 用新版拉起来。
// 真正有风险的那几步都在探针侧，见 internal/agent/upgrade.go 的注释。

// upgradeTimeout 是等一台机器升级完的时间。
// 下载 10MB 左右的二进制 + 校验，网络差的机器可能要一两分钟。
const upgradeTimeout = 200 * time.Second

// LatestAgentVersion 是这个面板认为的「最新版探针」。
//
// 它就是编译进面板的 internal/agent.Version —— 面板和探针在同一个仓库里，
// 一起发版，所以面板自己的版本号就是它该把节点升到的版本。
//
// 注意这只是**期望值**：data/agents/ 下的二进制是安装脚本单独下的，
// 理论上可能和面板不同版。所以升级完之后以探针实际报回来的版本为准，
// 而不是在这里假定成功。
func LatestAgentVersion() string { return agent.Version }

// upgradeNodeResult 是一台机器的升级结果。
type upgradeNodeResult struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	OK       bool   `json:"ok"`
	// Skipped 为真表示这台机器压根没动（已是最新，或者不在线）。
	Skipped     bool   `json:"skipped"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Message     string `json:"message"`
	Error       string `json:"error,omitempty"`
}

type upgradeRequest struct {
	// Force 为真时即使版本号已经一致也重装一遍。
	// 用于「二进制被改坏了」这种情况。
	Force bool `json:"force"`
}

// handleUpgradeNode 升级单台机器。
func (s *Server) handleUpgradeNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	var req upgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), upgradeTimeout+15*time.Second)
	defer cancel()

	res := s.upgradeNode(ctx, n, req.Force)
	if !res.OK && !res.Skipped {
		writeJSON(w, http.StatusBadGateway, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleUpgradeAll 把所有在线节点都升一遍。
//
// 串行做，不并发：升级要下载二进制，全部节点一起从面板拉会把面板那台机器的
// 上行带宽打满，反而每一台都变慢、甚至集体超时。节点数量级本来就是几十台，
// 串行完全够用。
func (s *Server) handleUpgradeAll(w http.ResponseWriter, r *http.Request) {
	var req upgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	nodes, err := s.st.ListNodes(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	// 整批的超时按节点数放大，但封顶 —— 一次点击不该让请求挂十分钟。
	total := time.Duration(len(nodes))*upgradeTimeout + 30*time.Second
	if total > 15*time.Minute {
		total = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), total)
	defer cancel()

	results := make([]upgradeNodeResult, 0, len(nodes))
	var upgraded, skipped, failed int

	for _, n := range nodes {
		if ctx.Err() != nil {
			results = append(results, upgradeNodeResult{
				NodeID: n.ID, NodeName: n.Name,
				Error: "整批升级已超时，这台没轮到，请单独重试",
			})
			failed++
			continue
		}
		res := s.upgradeNode(ctx, n, req.Force)
		results = append(results, res)
		switch {
		case res.Skipped:
			skipped++
		case res.OK:
			upgraded++
		default:
			failed++
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		// 失败的排前面：用户点完批量升级最想看的就是"谁没成"。
		return !results[i].OK && !results[i].Skipped &&
			(results[j].OK || results[j].Skipped)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"latest_version": LatestAgentVersion(),
		"upgraded":       upgraded,
		"skipped":        skipped,
		"failed":         failed,
		"results":        results,
	})
}

// upgradeNode 给一台机器下发升级指令并解析结果。
func (s *Server) upgradeNode(ctx context.Context, n *store.Node, force bool) upgradeNodeResult {
	latest := LatestAgentVersion()
	res := upgradeNodeResult{NodeID: n.ID, NodeName: n.Name, ToVersion: latest}

	live := s.hub.LiveOf(n.ID)
	if live != nil {
		res.FromVersion = live.AgentVersion
	}

	if !s.hub.Online(n.ID) {
		// 没有长连接不算失败：等它连回来再点一次就行。
		// 把它算成失败会让「批量升级」在有几台机器关着的时候永远显示红色。
		res.Skipped = true
		res.Message = "探针当前没有长连接，升级指令发不出去，已跳过。" +
			"机器刚重启或面板刚重启时会有一小段这样的窗口，等一会儿再试；" +
			"要是一直这样，多半是面板前面的反代没开 WebSocket Upgrade。"
		return res
	}
	if !force && res.FromVersion != "" && res.FromVersion == latest {
		res.Skipped = true
		res.OK = true
		res.Message = fmt.Sprintf("已经是 %s", latest)
		return res
	}

	cmd := protocol.Command{
		ID:         action.NewCommandID(n.ID),
		Cmd:        protocol.CmdUpgrade,
		TimeoutSec: int(upgradeTimeout / time.Second),
		Reason:     "面板下发的探针升级",
		Upgrade:    &protocol.UpgradeRequest{Version: latest, Force: force},
	}

	id := n.ID
	ack, err := s.hub.Send(ctx, n.ID, cmd)
	if err != nil {
		// 探针替换完二进制就退出了，连接跟着断。如果它是在回执发出去
		// **之前**断的，这里会拿到一个连接错误 —— 但升级其实可能已经成了。
		// 所以话不能说死，让用户看节点的版本号来确认。
		res.Error = err.Error()
		res.Message = "指令下发后连接就断了。探针替换完二进制会立刻重启，" +
			"这有可能是升级成功的正常现象 —— 等 30 秒看节点版本号有没有变上去。"
		if errors.Is(err, ErrAgentOffline) {
			res.Message = "探针在升级过程中掉线了。" + res.Message
		}
		s.st.AddEvent(ctx, &id, store.EventAgentUpgrade, store.LevelWarn,
			fmt.Sprintf("节点「%s」升级指令下发后失联：%v", n.Name, err))
		return res
	}

	var detail protocol.UpgradeResult
	if ack.Output != "" {
		_ = json.Unmarshal([]byte(ack.Output), &detail)
	}
	if detail.FromVersion != "" {
		res.FromVersion = detail.FromVersion
	}
	res.Message = detail.Message

	if !ack.OK {
		res.Error = ack.Error
		if res.Message == "" {
			res.Message = ack.Error
		}
		s.st.AddEvent(ctx, &id, store.EventAgentUpgrade, store.LevelError,
			fmt.Sprintf("节点「%s」升级失败：%s", n.Name, ack.Error))
		return res
	}

	res.OK = true
	res.Skipped = !detail.Replaced
	if res.Message == "" {
		res.Message = "已升级"
	}
	level, text := store.LevelInfo, fmt.Sprintf("节点「%s」探针已是最新版 %s", n.Name, latest)
	if detail.Replaced {
		level = store.LevelWarn
		text = fmt.Sprintf("节点「%s」探针已升级：%s → %s，正在重启",
			n.Name, detail.FromVersion, latest)
	}
	s.st.AddEvent(ctx, &id, store.EventAgentUpgrade, level, text)
	return res
}

// handleAgentVersions 汇报每台机器的探针版本和面板认为的最新版。
// 前端拿它在节点列表上标出"有新版本"。
func (s *Server) handleAgentVersions(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.st.ListNodes(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	latest := LatestAgentVersion()
	type row struct {
		NodeID   int64  `json:"node_id"`
		NodeName string `json:"node_name"`
		Version  string `json:"version"`
		Online   bool   `json:"online"`
		// Commandable 表示现在能不能给它下发指令。升级要走 WebSocket，
		// 探针降级成 HTTP 上报时它是假的 —— 那时候版本号照样能收到，
		// 但升级指令发不出去。前端据此把按钮置灰并说明原因，
		// 而不是把按钮整个藏起来让人无从下手。
		Commandable bool `json:"commandable"`
		// Outdated 只看版本号，不看在不在线 —— 「这台机器该升了」和
		// 「现在能不能升」是两回事，混在一起会让一台暂时没有长连接的机器
		// 从「待升级」列表里凭空消失。
		Outdated bool `json:"outdated"`
	}

	out := make([]row, 0, len(nodes))
	outdated, upgradable := 0, 0
	for _, n := range nodes {
		r := row{
			NodeID: n.ID, NodeName: n.Name,
			Online:      n.Status == store.StatusOnline,
			Commandable: s.hub.Online(n.ID),
		}
		if live := s.hub.LiveOf(n.ID); live != nil {
			r.Version = live.AgentVersion
		}
		r.Outdated = r.Version != "" && r.Version != latest
		if r.Outdated {
			outdated++
			if r.Commandable {
				upgradable++
			}
		}
		out = append(out, r)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"latest_version": latest,
		"outdated_count": outdated,
		// UpgradableCount 是「此刻真的升得动」的台数（版本旧 + 有长连接）。
		"upgradable_count": upgradable,
		"nodes":            out,
		// MissingBinaries 非空时升级一定会失败，先告诉用户。
		"missing_binaries": s.MissingAgentBinaries(),
	})
}
