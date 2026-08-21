package action

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/store"
)

// AgentCommander 是探针通道的抽象，由 WebSocket hub 实现。
// 抽成接口是为了让 action 包不依赖 HTTP 层，也方便单测替身。
type AgentCommander interface {
	// Online 判断探针当前是否有活跃连接。
	Online(nodeID int64) bool
	// Send 下发指令并等待执行结果。
	Send(ctx context.Context, nodeID int64, cmd protocol.Command) (*protocol.CommandResult, error)
}

// 通道选择。
const (
	ViaAuto  = ""      // 自动：优先探针，探针不可用时走 SSH
	ViaAgent = "agent" // 强制走探针
	ViaSSH   = "ssh"   // 强制走 SSH
)

// Outcome 是一次动作执行的结果。
type Outcome struct {
	Via    string `json:"via"`    // 实际走的通道
	Output string `json:"output"` // 命令输出
}

// Executor 编排"用哪条通道执行动作"。
type Executor struct {
	st    *store.Store
	ssh   *SSHRunner
	agent AgentCommander
}

func NewExecutor(st *store.Store, sshRunner *SSHRunner, agent AgentCommander) *Executor {
	return &Executor{st: st, ssh: sshRunner, agent: agent}
}

// SSH 暴露底层 runner，供「测试连接」等场景直接使用。
func (e *Executor) SSH() *SSHRunner { return e.ssh }

// Shutdown 关闭节点。via 指定首选通道，失败时自动尝试另一条。
func (e *Executor) Shutdown(ctx context.Context, n *store.Node, via, reason string) (*Outcome, error) {
	return e.run(ctx, n, via, reason, protocol.Command{
		Cmd:        protocol.CmdShutdown,
		Reason:     reason,
		TimeoutSec: 20,
	}, func(ctx context.Context) (string, string, error) {
		return e.ssh.Shutdown(ctx, n)
	})
}

// Exec 在节点上执行自定义命令。
func (e *Executor) Exec(ctx context.Context, n *store.Node, script, via, reason string) (*Outcome, error) {
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("命令为空")
	}
	return e.run(ctx, n, via, reason, protocol.Command{
		Cmd:        protocol.CmdExec,
		Script:     script,
		Reason:     reason,
		TimeoutSec: 30,
	}, func(ctx context.Context) (string, string, error) {
		return e.ssh.Exec(ctx, n, script)
	})
}

// run 是两条通道的公共调度逻辑。
func (e *Executor) run(
	ctx context.Context,
	n *store.Node,
	via, reason string,
	cmd protocol.Command,
	sshFn func(context.Context) (output, hostKey string, err error),
) (*Outcome, error) {
	agentUsable := e.agent != nil && e.agent.Online(n.ID)
	sshUsable := n.HasSSH()

	// 决定尝试顺序
	var order []string
	switch via {
	case ViaAgent:
		order = []string{ViaAgent}
		// 显式指定探针但探针离线时，仍然退到 SSH——用户的意图是"把这台机器关掉"，
		// 而不是"一定要用探针关"。
		if !agentUsable && sshUsable {
			order = append(order, ViaSSH)
		}
	case ViaSSH:
		order = []string{ViaSSH}
		if !sshUsable && agentUsable {
			order = append(order, ViaAgent)
		}
	default:
		order = []string{ViaAgent, ViaSSH}
	}

	var errs []string
	for _, ch := range order {
		switch ch {
		case ViaAgent:
			if !agentUsable {
				errs = append(errs, "探针通道: 探针当前离线")
				continue
			}
			cmd.ID = NewCommandID(n.ID)
			res, err := e.agent.Send(ctx, n.ID, cmd)
			if err != nil {
				errs = append(errs, "探针通道: "+err.Error())
				continue
			}
			if !res.OK {
				errs = append(errs, "探针通道: "+res.Error)
				continue
			}
			return &Outcome{Via: ViaAgent, Output: res.Output}, nil

		case ViaSSH:
			if !sshUsable {
				errs = append(errs, "SSH 通道: 节点未配置 SSH 凭据")
				continue
			}
			out, hostKey, err := sshFn(ctx)
			// TOFU：首次连接成功后把主机密钥记下来，之后严格校验
			if hostKey != "" && n.SSHHostKey == "" {
				if saveErr := e.st.SetNodeHostKey(ctx, n.ID, hostKey); saveErr == nil {
					n.SSHHostKey = hostKey
				}
			}
			if err != nil {
				// 关机命令把机器关掉后连接被掐断是正常现象，不算失败。
				if isBenignDisconnect(err) {
					return &Outcome{Via: ViaSSH, Output: out + "\n(连接在执行过程中断开，通常说明机器已开始关机)"}, nil
				}
				errs = append(errs, "SSH 通道: "+err.Error())
				continue
			}
			return &Outcome{Via: ViaSSH, Output: out}, nil
		}
	}

	return nil, fmt.Errorf("所有通道均失败 —— %s", strings.Join(errs, "; "))
}

// isBenignDisconnect 判断错误是不是"机器正在关机导致连接断开"。
func isBenignDisconnect(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, sig := range []string{
		"connection reset by peer",
		"unexpected EOF",
		"EOF",
		"broken pipe",
		"session channel closed",
		"connection lost",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// RunExceedAction 执行节点配置的超额动作。
// 返回 nil Outcome 表示这个动作不需要碰机器（none / dns_only）。
func (e *Executor) RunExceedAction(ctx context.Context, n *store.Node, reason string) (*Outcome, error) {
	switch n.ActionOnExceed {
	case store.ActionNone, store.ActionDNSOnly, "":
		return nil, nil

	case store.ActionShutdownAgent:
		return e.Shutdown(ctx, n, ViaAgent, reason)

	case store.ActionShutdownSSH:
		return e.Shutdown(ctx, n, ViaSSH, reason)

	case store.ActionCommand:
		if strings.TrimSpace(n.ExceedCommand) == "" {
			return nil, fmt.Errorf("超额动作设为「执行自定义命令」，但命令为空")
		}
		return e.Exec(ctx, n, n.ExceedCommand, ViaAuto, reason)

	default:
		return nil, fmt.Errorf("未知的超额动作 %q", n.ActionOnExceed)
	}
}

// ValidAction 判断超额动作是否合法。
func ValidAction(a string) bool {
	switch a {
	case store.ActionNone, store.ActionDNSOnly, store.ActionShutdownAgent,
		store.ActionShutdownSSH, store.ActionCommand:
		return true
	}
	return false
}

// StopsMachine 判断某个动作会不会让机器停止服务，
// 用来决定执行后节点状态该置成 stopped 还是仅 exceeded。
func StopsMachine(a string) bool {
	switch a {
	case store.ActionShutdownAgent, store.ActionShutdownSSH, store.ActionCommand:
		return true
	}
	return false
}

var cmdSeq atomic.Uint64

// NewCommandID 生成全局唯一的指令 ID，探针用它把结果对应回请求。
// 导出是因为面板的转发链路测试也要直接走 Hub.Send，而 Hub 要求指令带 ID。
func NewCommandID(nodeID int64) string {
	return fmt.Sprintf("%d-%d-%d", nodeID, time.Now().UnixNano(), cmdSeq.Add(1))
}
