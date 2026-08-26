package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/selfupdate"
)

// 探针自升级。
//
// 目标是让用户在面板上点一下就能把所有节点升到最新版，不用挨台 SSH。
//
// 真正危险的那几步（下载到同目录临时文件、校验能不能跑、备份后原子替换、
// 清理残留）全在 internal/selfupdate 里，面板自更新用的是同一份代码。
// 这里只负责：从面板下载、把结果回给面板、然后退出让 systemd 拉起。
//
// 有一条顺序不能反：**先把结果发出去，再退出**。反过来的话，面板永远只会
// 看到「指令超时」，哪怕升级其实成功了。

// upgradeDownloadTimeout 是下载新二进制的超时。探针二进制约 10MB，
// 给足时间让网络差的机器也能下完。
const upgradeDownloadTimeout = 3 * time.Minute

// upgradeExitDelay 是回复面板之后、真正退出之前的等待。
// 给 WebSocket 写循环留出把结果发出去的时间。
const upgradeExitDelay = 1500 * time.Millisecond

// minAgentBinarySize 是一个合理的下限。探针是静态编译的 Go 程序，
// 再怎么也有几 MB；比这还小的一定不是它（多半是错误页面）。
const minAgentBinarySize = 2 << 20

// tmpBinaryPrefix 是下载中的临时文件前缀。开头的点让它在 ls 里不碍眼，
// 也方便清理时认出哪些是自己留下的。
const tmpBinaryPrefix = ".vps-agent-new-"

// upgradeTarget 描述「怎么替换探针自己的二进制、怎么算校验通过」。
var upgradeTarget = selfupdate.Target{
	ExpectOutput: "vps-agent",
	MinSize:      minAgentBinarySize,
	TempPrefix:   tmpBinaryPrefix,
}

// upgrade 执行一次自升级。
func (a *Agent) upgrade(ctx context.Context, req *protocol.UpgradeRequest) protocol.CommandResult {
	if req == nil {
		req = &protocol.UpgradeRequest{}
	}
	res := protocol.UpgradeResult{FromVersion: Version, ToVersion: req.Version}

	if !a.cfg.AllowUpgrade {
		return upgradeFailed(res, errors.New(
			"该探针启动时禁用了远程升级（--allow-upgrade=false）"))
	}

	// 已经是目标版本就别白折腾一次重启。面板批量升级时，
	// 大部分节点本来就已经是最新的。
	if req.Version != "" && req.Version == Version && !req.Force {
		res.Message = fmt.Sprintf("已经是 %s，无需升级", Version)
		return upgradeOK(res)
	}

	exePath, err := upgradeTarget.Path()
	if err != nil {
		return upgradeFailed(res, err)
	}
	res.BinaryPath = exePath

	a.log.Warn("收到面板下发的升级指令", "当前版本", Version, "目标版本", req.Version, "二进制", exePath)

	body, err := a.fetchAgentBinary(ctx)
	if err != nil {
		return upgradeFailed(res, err)
	}
	defer body.Close()

	out, err := upgradeTarget.Apply(ctx, body, a.log)
	if err != nil {
		return upgradeFailed(res, err)
	}

	res.SizeBytes = out.SizeBytes
	res.Replaced = true
	res.Restarting = true
	res.Message = fmt.Sprintf("已替换 %s（%.1f MB），探针即将退出，由 systemd 用新版本拉起",
		out.BinaryPath, float64(out.SizeBytes)/(1<<20))

	a.log.Warn("升级完成，即将退出让 systemd 重启", "二进制", out.BinaryPath, "大小", out.SizeBytes)

	// 先让结果发出去，再退出。反过来的话面板只会看到超时。
	go a.exitForUpgrade()

	return upgradeOK(res)
}

// exitForUpgrade 在把结果发回面板之后退出进程。
//
// 用退出而不是 exec 自己：systemd 的 Restart=always 会把它拉起来，
// 而且是一个干净的新进程 —— 转发规则、连接池这些都会按正常启动流程重建，
// 不用担心 exec 之后继承下来的一堆 fd 和半开连接。
//
// 退出码用 0：非 0 会被 systemd 记成失败，日志里一片红，
// 而这其实是一次完全正常的计划内重启。
func (a *Agent) exitForUpgrade() {
	time.Sleep(upgradeExitDelay)
	// 走一次正常的收尾：把最后一段流量补报上去再退出，
	// 和收到 SIGTERM 时的处理保持一致，这样升级不会丢流量。
	a.finalFlush()
	osExit(0)
}

// osExit 做成变量只为让单测能拦住它，别把测试进程本身退掉。
var osExit = os.Exit

// fetchAgentBinary 从面板拉对应架构的探针二进制，返回响应体供调用方消费。
func (a *Agent) fetchAgentBinary(ctx context.Context) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s?arch=%s", a.downloadEndpoint(), runtime.GOARCH)

	dlCtx, cancel := context.WithTimeout(ctx, upgradeDownloadTimeout)
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("X-Node-Secret", a.cfg.Secret)

	resp, err := a.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("从面板下载新版探针失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("面板返回 HTTP %d：%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// 把 cancel 挂到 Close 上，别让 context 提前取消把下载掐断。
	return &bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}, nil
}

type bodyWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bodyWithCancel) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// downloadEndpoint 拼出面板的二进制下载地址。
func (a *Agent) downloadEndpoint() string {
	if a.secure.Load() {
		return "https://" + a.host + "/agent/download"
	}
	return "http://" + a.host + "/agent/download"
}

// cleanupUpgradeLeftovers 清掉上次升级留下的半截文件。探针启动时调一次。
func cleanupUpgradeLeftovers(log *slog.Logger) {
	upgradeTarget.CleanupLeftovers(log)
}

func upgradeOK(res protocol.UpgradeResult) protocol.CommandResult {
	return protocol.CommandResult{OK: true, Output: marshalUpgrade(res)}
}

func upgradeFailed(res protocol.UpgradeResult, err error) protocol.CommandResult {
	res.Message = err.Error()
	return protocol.CommandResult{OK: false, Error: err.Error(), Output: marshalUpgrade(res)}
}

// marshalUpgrade 把结果序列化进 CommandResult.Output。
// 序列化失败时退回一句纯文本 —— 结构化信息没了不要紧，
// 但「升级到底成没成」这句话一定要能传回面板。
func marshalUpgrade(res protocol.UpgradeResult) string {
	b, err := json.Marshal(res)
	if err != nil {
		return res.Message
	}
	return string(b)
}
