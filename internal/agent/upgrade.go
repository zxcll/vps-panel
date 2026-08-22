package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/protocol"
)

// 探针自升级。
//
// 目标是让用户在面板上点一下就能把所有节点升到最新版，不用挨台 SSH。
//
// 整件事的核心风险只有一个：**把自己的二进制换坏了，探针就再也起不来了**，
// 而这台机器很可能远在天边、SSH 凭据还未必配过。所以这里的每一步都必须
// 能安全失败 —— 任何一步出问题，都要保证旧二进制原封不动。
//
// 顺序是这样的：
//
//  1. 下载到**同目录**下的临时文件（同目录才能 rename，跨文件系统 rename 会失败）
//  2. 校验它确实是个能跑的东西：ELF 魔数 + 实际执行 `--version`
//  3. 备份旧二进制到 .old，再 rename 新的上去（rename 在同文件系统上是原子的）
//  4. 先把结果回给面板，**然后**才退出
//
// 第 2 步是关键。只校验「下下来了」是不够的：反代返回一个 HTML 错误页、
// 面板上放错了架构的二进制、传输被截断，这几种都能让你下到一个"看着有内容"
// 但根本跑不起来的文件。真的执行一次 --version 是唯一能排除全部这些的办法。
//
// 第 4 步同样重要。反过来的话，面板永远只会看到"指令超时"，
// 哪怕升级其实成功了。

// upgradeDownloadTimeout 是下载新二进制的超时。探针二进制约 10MB，
// 给足时间让网络差的机器也能下完。
const upgradeDownloadTimeout = 3 * time.Minute

// upgradeExitDelay 是回复面板之后、真正退出之前的等待。
// 给 WebSocket 写循环留出把结果发出去的时间。
const upgradeExitDelay = 1500 * time.Millisecond

// minAgentBinarySize 是一个合理的下限。探针是静态编译的 Go 程序，
// 再怎么也有几 MB；比这还小的一定不是它（多半是错误页面）。
const minAgentBinarySize = 2 << 20

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

	exePath, err := currentBinaryPath()
	if err != nil {
		return upgradeFailed(res, err)
	}
	res.BinaryPath = exePath

	a.log.Warn("收到面板下发的升级指令", "当前版本", Version, "目标版本", req.Version, "二进制", exePath)

	tmpPath, size, err := a.downloadAgentBinary(ctx, exePath)
	if err != nil {
		return upgradeFailed(res, err)
	}
	// 从这里开始，任何失败都要把临时文件清掉，旧二进制保持原样。
	defer os.Remove(tmpPath)

	res.SizeBytes = size

	if err := verifyAgentBinary(ctx, tmpPath); err != nil {
		return upgradeFailed(res, err)
	}
	if err := swapBinary(tmpPath, exePath); err != nil {
		return upgradeFailed(res, err)
	}

	res.Replaced = true
	res.Restarting = true
	res.Message = fmt.Sprintf("已替换 %s（%.1f MB），探针即将退出，由 systemd 用新版本拉起",
		exePath, float64(size)/(1<<20))

	a.log.Warn("升级完成，即将退出让 systemd 重启", "二进制", exePath, "大小", size)

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
	os.Exit(0)
}

// currentBinaryPath 找到当前进程的二进制路径。
//
// 要解符号链接：有些安装方式会把 /usr/local/bin/vps-agent 指到别处，
// 直接往符号链接上 rename 会把链接本身替换掉，下次升级就找不到真身了。
func currentBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位探针自身的二进制路径失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// downloadAgentBinary 从面板下载对应架构的二进制，落到 exePath 的同目录下。
//
// 必须同目录：rename 只在同一个文件系统内是原子的。下到 /tmp 再 rename
// 到 /usr/local/bin，在 /tmp 是 tmpfs 的机器上会直接失败。
func (a *Agent) downloadAgentBinary(ctx context.Context, exePath string) (string, int64, error) {
	url := fmt.Sprintf("%s?arch=%s", a.downloadEndpoint(), runtime.GOARCH)

	dlCtx, cancel := context.WithTimeout(ctx, upgradeDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("X-Node-Secret", a.cfg.Secret)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("从面板下载新版探针失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", 0, fmt.Errorf("面板返回 HTTP %d：%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".vps-agent-new-*")
	if err != nil {
		return "", 0, fmt.Errorf("在 %s 下创建临时文件失败: %w（这个目录可写吗？探针是以 root 跑的吗？）", dir, err)
	}
	tmpPath := tmp.Name()

	size, err := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("写入新版探针失败: %w", err)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("关闭临时文件失败: %w", closeErr)
	}

	if size < minAgentBinarySize {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("下到的文件只有 %d 字节，不可能是探针二进制"+
			"（面板上放的可能是错误页面，或者 %s 架构的二进制没准备好）", size, runtime.GOARCH)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("给新版探针加执行权限失败: %w", err)
	}
	return tmpPath, size, nil
}

// downloadEndpoint 拼出面板的二进制下载地址。
func (a *Agent) downloadEndpoint() string {
	if a.secure.Load() {
		return "https://" + a.host + "/agent/download"
	}
	return "http://" + a.host + "/agent/download"
}

// elfMagic 是 ELF 文件的前四个字节。
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// verifyAgentBinary 确认这个文件真的是一个能跑的探针。
//
// 两道关：
//
//   - ELF 魔数：挡掉反代返回的 HTML 错误页、下了一半的文件。
//   - 真的执行一次 `--version`：这才是决定性的一道。架构不对
//     （给 arm64 机器下了 amd64 的）、动态链接缺库、文件被截断，
//     都只有真跑一次才暴露得出来。
//
// 少了第二道，最坏情况是把一个跑不起来的东西装上去，然后 systemd
// 每 5 秒重启一次、永远起不来 —— 而机器在天边，只能 SSH 上去手工救。
func verifyAgentBinary(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开新版探针失败: %w", err)
	}
	head := make([]byte, 4)
	n, readErr := io.ReadFull(f, head)
	f.Close()
	if readErr != nil || n < 4 {
		return fmt.Errorf("读不出新版探针的文件头，下载可能不完整")
	}
	if string(head) != string(elfMagic) {
		return fmt.Errorf("下到的不是 Linux 可执行文件（文件头是 %q）"+
			"，多半是反代返回了错误页面而不是二进制", string(head))
	}

	verCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := exec.CommandContext(verCtx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("新版探针跑不起来（%w），已放弃升级，旧版本保持原样。"+
			"输出：%s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "vps-agent") {
		return fmt.Errorf("新版探针的 --version 输出对不上（%q），"+
			"面板上放的可能不是这个项目的二进制",
			strings.TrimSpace(string(out)))
	}
	return nil
}

// swapBinary 把新二进制换上去，旧的留一份 .old。
//
// 留 .old 是为了救命：真出了「新版起不来」的情况，SSH 上去
// `mv vps-agent.old vps-agent` 就能立刻回滚，不用重新跑安装脚本、
// 更不用在一台已经连不上面板的机器上想办法联网。
//
// 注意用 rename 而不是覆盖写：Linux 不允许写一个正在执行的文件
// （ETXTBSY），但允许 rename 掉它 —— 老的 inode 会一直活到进程退出。
func swapBinary(tmpPath, exePath string) error {
	backup := exePath + ".old"
	// 备份失败不致命：升级本身还是能做的，只是没了回滚的便利。
	_ = os.Remove(backup)
	_ = os.Link(exePath, backup)

	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("替换探针二进制失败: %w（%s 可写吗？探针是以 root 跑的吗？）",
			err, filepath.Dir(exePath))
	}
	return nil
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
