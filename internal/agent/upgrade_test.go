package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 这一组用例守的是同一件事：**任何一步失败，旧二进制都必须原封不动**。
//
// 探针把自己的二进制换坏了，机器就再也连不上面板了，而那台机器很可能
// 远在天边、SSH 凭据还未必配过。所以这里宁可漏升级，也不能装坏。

// fakeBinary 造一个能跑的假"探针"，--version 时输出指定内容。
func fakeBinary(t *testing.T, dir, versionOutput string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("自升级只在 Linux 上有意义")
	}

	// 用 shell 脚本冒充：它不是 ELF，所以只能用来测「跑得起来」那一关。
	// ELF 那一关单独用真的二进制测。
	path := filepath.Join(dir, "fake-agent")
	script := "#!/bin/sh\necho '" + versionOutput + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写假二进制失败: %v", err)
	}
	return path
}

// realBinary 编译一个真的 ELF 出来。verify 的两关都要过它才算数。
func realBinary(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("自升级只在 Linux 上有意义")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境里没有 go，跳过需要真实编译的用例")
	}

	src := filepath.Join(dir, "main.go")
	code := `package main

import "fmt"

func main() { fmt.Println("vps-agent 9.9.9") }
`
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatalf("写源码失败: %v", err)
	}
	out := filepath.Join(dir, "real-agent")
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("编译测试二进制失败，跳过: %v\n%s", err, b)
	}
	return out
}

func TestVerifyRejectsNonELF(t *testing.T) {
	dir := t.TempDir()
	// 反代配错时最典型的情况：下回来一个 HTML 错误页。
	path := filepath.Join(dir, "notelf")
	if err := os.WriteFile(path, []byte("<html>502 Bad Gateway</html>"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := verifyAgentBinary(context.Background(), path)
	if err == nil {
		t.Fatal("非 ELF 文件必须被拒绝")
	}
	if !strings.Contains(err.Error(), "可执行文件") {
		t.Errorf("错误消息应说清楚是「不是可执行文件」，实际：%v", err)
	}
}

func TestVerifyRejectsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny")
	// 连 4 字节文件头都不够。
	if err := os.WriteFile(path, []byte("EL"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyAgentBinary(context.Background(), path); err == nil {
		t.Fatal("读不出文件头的必须被拒绝")
	}
}

// 关键的一关：文件是 ELF、也跑得起来，但它不是这个项目的东西。
//
// 只校验 ELF 魔数是不够的 —— 面板上放错了别的程序、或者放了另一个
// 架构的二进制，魔数都是对的。
func TestVerifyRejectsWrongProgram(t *testing.T) {
	dir := t.TempDir()
	// /bin/true 是个货真价实的 ELF，也能跑，但它不是探针。
	real, err := exec.LookPath("true")
	if err != nil {
		t.Skip("环境里没有 /bin/true")
	}
	copied := filepath.Join(dir, "true-copy")
	data, err := os.ReadFile(real)
	if err != nil {
		t.Skipf("读不了 %s: %v", real, err)
	}
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}

	err = verifyAgentBinary(context.Background(), copied)
	if err == nil {
		t.Fatal("跑得起来但不是探针的程序必须被拒绝")
	}
	if !strings.Contains(err.Error(), "--version") && !strings.Contains(err.Error(), "对不上") {
		t.Errorf("错误消息应指向 --version 校验，实际：%v", err)
	}
}

func TestVerifyAcceptsRealAgentBinary(t *testing.T) {
	dir := t.TempDir()
	path := realBinary(t, dir)

	if err := verifyAgentBinary(context.Background(), path); err != nil {
		t.Errorf("一个真的、--version 输出正确的二进制应当通过，实际报错：%v", err)
	}
}

// 假二进制（shell 脚本）能跑、输出也对，但不是 ELF —— 必须被 ELF 那一关挡住。
// 这条用例的意义是确认两关是**都要过**，不是过一个就行。
func TestVerifyRequiresBothChecks(t *testing.T) {
	dir := t.TempDir()
	path := fakeBinary(t, dir, "vps-agent 1.2.3")

	if err := verifyAgentBinary(context.Background(), path); err == nil {
		t.Error("shell 脚本虽然跑得起来、输出也对，但不是 ELF，应当被拒绝")
	}
}

func TestSwapBinaryKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "vps-agent")
	if err := os.WriteFile(exePath, []byte("老版本"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(dir, ".new")
	if err := os.WriteFile(tmpPath, []byte("新版本"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(tmpPath, exePath); err != nil {
		t.Fatalf("替换失败: %v", err)
	}

	got, _ := os.ReadFile(exePath)
	if string(got) != "新版本" {
		t.Errorf("没换上新版本，实际内容 %q", got)
	}

	// .old 是救命用的：新版起不来时 SSH 上去 mv 回来就能立刻回滚。
	backup, err := os.ReadFile(exePath + ".old")
	if err != nil {
		t.Fatalf("应留下 .old 备份供回滚: %v", err)
	}
	if string(backup) != "老版本" {
		t.Errorf(".old 里应是旧二进制，实际 %q", backup)
	}
}

// 连着升两次也要正常：第二次的 .old 应该被覆盖，而不是因为已存在而失败。
func TestSwapBinaryTwice(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "vps-agent")
	os.WriteFile(exePath, []byte("v1"), 0o755)

	for _, v := range []string{"v2", "v3"} {
		tmp := filepath.Join(dir, ".new-"+v)
		os.WriteFile(tmp, []byte(v), 0o755)
		if err := swapBinary(tmp, exePath); err != nil {
			t.Fatalf("升到 %s 失败: %v", v, err)
		}
	}

	got, _ := os.ReadFile(exePath)
	if string(got) != "v3" {
		t.Errorf("最终应是 v3，实际 %q", got)
	}
	backup, _ := os.ReadFile(exePath + ".old")
	if string(backup) != "v2" {
		t.Errorf(".old 应是上一版 v2，实际 %q", backup)
	}
}

// 关掉 --allow-upgrade 之后，面板推升级必须被拒。
func TestUpgradeRefusedWhenDisabled(t *testing.T) {
	a := &Agent{cfg: Config{AllowUpgrade: false}, log: quietLogger()}

	res := a.upgrade(context.Background(), nil)
	if res.OK {
		t.Fatal("禁用远程升级后不该执行")
	}
	if !strings.Contains(res.Error, "allow-upgrade") {
		t.Errorf("错误消息应指出是哪个开关关着，实际：%s", res.Error)
	}
}
