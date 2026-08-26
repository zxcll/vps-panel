// Package selfupdate 把「一个正在运行的程序替换掉自己的二进制」这件事做对。
//
// 面板和探针都要做这件事，做法完全一样，所以抽出来共用 —— **不是**各写一份。
// 这段代码换错了就是把程序自己砸了：探针砸了丢一台机器的监控，面板砸了整个
// 控制面都没了。两份实现迟早会分叉，而分叉出去的那一份一定是没人测的那份。
//
// 整件事的核心风险只有一个：**新二进制跑不起来**。而那台机器很可能远在天边、
// SSH 凭据还未必配过。所以每一步都必须能安全失败 —— 任何一步出问题，
// 都要保证旧二进制原封不动。
//
// 顺序：
//
//  1. 写到**同目录**下的临时文件（同目录才能 rename，跨文件系统会失败）
//  2. 校验它确实是个能跑的东西：ELF 魔数 + 实际执行 `--version`
//  3. 备份旧二进制到 .old，再 rename 新的上去（同文件系统上 rename 是原子的）
//  4. 调用方**先把结果回给对端，然后**才退出
//
// 第 2 步是关键。只校验「下下来了」远远不够：反代返回一个 HTML 错误页、
// 放错了架构的二进制、传输被截断，这几种都能产生一个"看着有内容"但根本跑不起来
// 的文件。实测中「是 ELF 但一跑就段错误」这种恰恰是魔数拦不住的。
// 真跑一次 --version 是唯一能排除全部情况的办法。
package selfupdate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// verifyTimeout 是执行 --version 的超时。
const verifyTimeout = 20 * time.Second

// elfMagic 是 ELF 文件的前四个字节。
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// Target 描述「要替换哪个二进制、怎么算校验通过」。
type Target struct {
	// BinaryPath 是要替换的文件。留空表示替换当前进程自己的二进制。
	BinaryPath string
	// VersionFlag 是用来验证的参数，留空即 --version。
	VersionFlag string
	// ExpectOutput 是 --version 的输出里必须出现的字符串，比如 "vps-panel"。
	// 它挡的是「文件是 ELF、也跑得起来，但压根不是这个程序」——
	// 面板上放错了别的二进制时就是这种。
	ExpectOutput string
	// MinSize 是合理的字节数下限。静态编译的 Go 程序再小也有几 MB，
	// 比这还小的一定不是它（多半是错误页面）。
	MinSize int64
	// TempPrefix 是临时文件前缀，开头带点，方便 CleanupLeftovers 认出来。
	TempPrefix string
}

// Result 是一次成功替换的结果。
type Result struct {
	BinaryPath string
	BackupPath string
	SizeBytes  int64
}

func (t Target) versionFlag() string {
	if t.VersionFlag == "" {
		return "--version"
	}
	return t.VersionFlag
}

func (t Target) tempPrefix() string {
	if t.TempPrefix == "" {
		return ".selfupdate-new-"
	}
	return t.TempPrefix
}

// Path 解出要替换的二进制路径。
//
// 要解符号链接：有些安装方式会把 /usr/local/bin/xxx 指到别处，
// 直接往符号链接上 rename 会把链接本身替换掉，下次升级就找不到真身了。
func (t Target) Path() (string, error) {
	p := t.BinaryPath
	if p == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("定位自身的二进制路径失败: %w", err)
		}
		p = exe
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// Apply 把 src 里的内容作为新二进制装上去。
//
// src 通常是一个 HTTP 响应体。函数不负责关闭它。
// 返回成功即表示新二进制已经就位，调用方接下来该回复对端、然后退出进程。
func (t Target) Apply(ctx context.Context, src io.Reader, log *slog.Logger) (Result, error) {
	exePath, err := t.Path()
	if err != nil {
		return Result{}, err
	}

	// 先把上次可能留下的半截文件清掉，别让反复失败的升级把磁盘堆满。
	t.CleanupLeftovers(log)

	tmpPath, size, err := t.stage(exePath, src)
	if err != nil {
		return Result{}, err
	}
	// 从这里开始，任何失败都要把临时文件清掉，旧二进制保持原样。
	defer os.Remove(tmpPath)

	if err := t.Verify(ctx, tmpPath); err != nil {
		return Result{}, err
	}
	backup, err := swap(tmpPath, exePath, log)
	if err != nil {
		return Result{}, err
	}
	return Result{BinaryPath: exePath, BackupPath: backup, SizeBytes: size}, nil
}

// stage 把内容写进 exePath **同目录**下的临时文件。
//
// 必须同目录：rename 只在同一个文件系统内是原子的。写到 /tmp 再 rename 到
// /usr/local/bin，在 /tmp 是 tmpfs 的机器上会直接失败。
func (t Target) stage(exePath string, src io.Reader) (string, int64, error) {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, t.tempPrefix()+"*")
	if err != nil {
		return "", 0, fmt.Errorf("在 %s 下创建临时文件失败: %w（这个目录可写吗？进程是以 root 跑的吗？）", dir, err)
	}
	tmpPath := tmp.Name()

	size, copyErr := io.Copy(tmp, src)
	closeErr := tmp.Close()

	fail := func(format string, args ...any) (string, int64, error) {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf(format, args...)
	}
	if copyErr != nil {
		return fail("写入新二进制失败: %w", copyErr)
	}
	if closeErr != nil {
		return fail("关闭临时文件失败: %w", closeErr)
	}
	if t.MinSize > 0 && size < t.MinSize {
		return fail("下到的文件只有 %d 字节，不可能是完整的二进制"+
			"（多半是反代返回了错误页面，或者对应架构的文件没准备好）", size)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fail("给新二进制加执行权限失败: %w", err)
	}
	return tmpPath, size, nil
}

// Verify 确认这个文件真的是一个能跑的、正确的程序。
//
// 两道关**都要过**：
//
//   - ELF 魔数：挡掉反代返回的 HTML 错误页、下了一半的文件。
//   - 真的执行一次 --version：这才是决定性的一道。架构不对（给 arm64 机器
//     下了 amd64 的）、动态链接缺库、文件被截断，都只有真跑一次才暴露得出来。
//
// 少了第二道，最坏情况是把一个跑不起来的东西装上去，然后 systemd 每 5 秒
// 重启一次、永远起不来 —— 而机器在天边，只能 SSH 上去手工救。
func (t Target) Verify(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开新二进制失败: %w", err)
	}
	head := make([]byte, 4)
	n, readErr := io.ReadFull(f, head)
	f.Close()
	if readErr != nil || n < 4 {
		return fmt.Errorf("读不出新二进制的文件头，下载可能不完整")
	}
	if string(head) != string(elfMagic) {
		return fmt.Errorf("下到的不是 Linux 可执行文件（文件头是 %q），"+
			"多半是反代返回了错误页面而不是二进制", string(head))
	}

	verCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	out, err := exec.CommandContext(verCtx, path, t.versionFlag()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("新二进制跑不起来（%w），已放弃升级，旧版本保持原样。输出：%s",
			err, strings.TrimSpace(string(out)))
	}
	if t.ExpectOutput != "" && !strings.Contains(string(out), t.ExpectOutput) {
		return fmt.Errorf("新二进制的 %s 输出对不上（%q），期望里面含有 %q —— "+
			"放的可能不是这个程序的二进制",
			t.versionFlag(), strings.TrimSpace(string(out)), t.ExpectOutput)
	}
	return nil
}

// swap 把新二进制换上去，旧的留一份 .old，返回备份路径。
//
// 留 .old 是为了救命：真出了「新版起不来」的情况，SSH 上去
// `mv xxx.old xxx` 就能立刻回滚，不用重新跑安装脚本、更不用在一台已经
// 连不上的机器上想办法联网。
//
// **只留一份。** 每次替换前先把上一份删掉，所以升多少次都只多占一个二进制的
// 空间，不会随升级次数越堆越多。
//
// 注意用 rename 而不是覆盖写：Linux 不允许写一个正在执行的文件（ETXTBSY），
// 但允许 rename 掉它 —— 老的 inode 会一直活到进程退出。
func swap(tmpPath, exePath string, log *slog.Logger) (string, error) {
	backup := exePath + ".old"
	// 用硬链接而不是复制：链接不额外占空间，等下面 rename 把原名挪走之后，
	// 那个 inode 就靠 .old 这个名字继续活着，正好是我们要的。
	_ = os.Remove(backup)
	if err := os.Link(exePath, backup); err != nil {
		// 备份失败不致命，升级照做，只是少了本地回滚的便利。
		if log != nil {
			log.Warn("留旧版本备份失败，升级继续但没有本地回滚点", "备份路径", backup, "err", err)
		}
		backup = ""
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		return "", fmt.Errorf("替换二进制失败: %w（%s 可写吗？进程是以 root 跑的吗？）",
			err, filepath.Dir(exePath))
	}
	return backup, nil
}

// CleanupLeftovers 清掉上次升级留下的临时文件。
//
// 正常路径上它们要么被 rename 成正式二进制、要么在失败时被删掉；但**进程在
// 下载中途被杀**（机器重启、OOM、systemd stop）就没人收拾了，反复几次就会在
// 二进制目录下堆出一串七八兆的碎片。
//
// 进程启动时和每次升级前各调一次。启动时调是安全的：那会儿不可能有正在进行的
// 升级，凡是留在那儿的临时文件都是孤儿。
func (t Target) CleanupLeftovers(log *slog.Logger) {
	exePath, err := t.Path()
	if err != nil {
		return
	}
	CleanupIn(filepath.Dir(exePath), t.tempPrefix(), log)
}

// CleanupIn 清掉某个目录下所有指定前缀的临时文件。拆出来是为了能单测。
func CleanupIn(dir, prefix string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil {
			if log != nil {
				log.Debug("清理升级残留失败", "文件", p, "err", err)
			}
			continue
		}
		if log != nil {
			log.Info("清掉了上次升级留下的临时文件", "文件", p)
		}
	}
}
