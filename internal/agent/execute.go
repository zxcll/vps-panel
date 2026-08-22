package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
)

// shutdownChain 串联多个关机命令。不同发行版、精简镜像里能用的不一样，
// 一个个试过去总有一个成。
const shutdownChain = `shutdown -h now || poweroff || systemctl poweroff || halt -p || busybox poweroff -f`

// execute 执行面板下发的一条指令。
func (a *Agent) execute(ctx context.Context, cmd protocol.Command) protocol.CommandResult {
	res := protocol.CommandResult{ID: cmd.ID}

	switch cmd.Cmd {
	case protocol.CmdReport:
		a.TriggerFlush()
		res.OK = true
		res.Output = "已触发一次立即上报"

	case protocol.CmdShutdown:
		a.log.Warn("收到面板下发的关机指令", "原因", cmd.Reason)
		res.OK = true
		res.Output = "已接受关机指令，将在补报流量后执行关机"
		// 关键顺序：先把最后一段流量报上去，再关机。
		// 反过来的话这段流量就永远丢了。
		go a.shutdown(cmd.Reason)

	case protocol.CmdForwardTest:
		// 只是拨个号 + 读几个只读状态，不执行任何命令，
		// 所以不受 --allow-exec 约束，只看转发功能本身开没开。
		if a.fwd == nil {
			res.Error = "该探针启动时禁用了端口转发（--allow-forward=false）"
			return res
		}
		req := cmd.Probe
		if req == nil {
			req = &protocol.ProbeRequest{}
		}
		probe := a.fwd.Probe(ctx, strings.TrimSpace(req.Target))
		if req.ListenPort > 0 {
			probe.Listening = a.fwd.ProbeListen(req.ListenPort)
		}
		out, err := json.Marshal(probe)
		if err != nil {
			res.Error = "序列化探测结果失败: " + err.Error()
			return res
		}
		res.Output = string(out)
		res.OK = true

	case protocol.CmdUpgrade:
		// 结果里的 ID 要自己补上：upgrade 返回的是一个新构造的
		// CommandResult，没经过上面那个 res。
		out := a.upgrade(ctx, cmd.Upgrade)
		out.ID = cmd.ID
		return out

	case protocol.CmdExec:
		if !a.cfg.AllowExec {
			res.Error = "该探针启动时禁用了自定义命令执行（--allow-exec=false）"
			return res
		}
		if strings.TrimSpace(cmd.Script) == "" {
			res.Error = "命令为空"
			return res
		}
		a.log.Warn("收到面板下发的命令", "命令", cmd.Script, "原因", cmd.Reason)

		timeout := time.Duration(cmd.TimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		out, err := runShell(ctx, cmd.Script, timeout)
		res.Output = out
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
		}

	default:
		res.Error = "未知指令: " + cmd.Cmd
	}

	return res
}

// shutdown 先补报流量，再关机。
func (a *Agent) shutdown(reason string) {
	// 给回执帧一点时间发出去，免得面板那边只看到连接断开
	time.Sleep(500 * time.Millisecond)

	a.log.Warn("正在补报最后一次流量，随后关机")
	a.finalFlush()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	out, err := runShell(ctx, shutdownChain, 20*time.Second)
	if err != nil {
		a.log.Error("关机命令执行失败", "err", err, "输出", out,
			"提示", "探针可能不是以 root 运行；可以改用面板的 SSH 远程关机")
		return
	}
	a.log.Warn("关机命令已执行", "原因", reason, "输出", out)
}

// runShell 用 /bin/sh 执行命令，合并 stdout 和 stderr。
func runShell(parent context.Context, script string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("命令执行超过 %s 未结束，已终止", timeout)
	}
	if err != nil {
		return text, fmt.Errorf("命令返回错误: %w", err)
	}
	return text, nil
}
