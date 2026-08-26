package selfupdate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 这一组守的是同一件事：**任何一步失败，旧二进制都必须原封不动**。
//
// 程序把自己的二进制换坏了，就再也起不来了：探针砸了丢一台机器的监控，
// 面板砸了整个控制面都没了。而那台机器很可能远在天边、SSH 凭据还未必配过。
// 所以宁可漏升级，也不能装坏。

func testTarget() Target {
	return Target{ExpectOutput: "vps-agent", MinSize: 0, TempPrefix: ".test-new-"}
}

// realBinary 编译一个真的 ELF 出来。verify 的两关都要过它才算数。
func realBinary(t *testing.T, dir, output string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("自更新只在 Linux 上有意义")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境里没有 go，跳过需要真实编译的用例")
	}

	src := filepath.Join(dir, "main.go")
	code := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"" + output + "\") }\n"
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatalf("写源码失败: %v", err)
	}
	out := filepath.Join(dir, "real-bin")
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
	os.WriteFile(path, []byte("<html>502 Bad Gateway</html>"), 0o755)

	err := testTarget().Verify(context.Background(), path)
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
	os.WriteFile(path, []byte("EL"), 0o755) // 连 4 字节文件头都不够

	if err := testTarget().Verify(context.Background(), path); err == nil {
		t.Fatal("读不出文件头的必须被拒绝")
	}
}

// 关键的一关：文件是 ELF、也跑得起来，但它不是我们要的那个程序。
//
// 只校验 ELF 魔数是不够的 —— 面板上放错了别的程序、或者放了另一个架构的
// 二进制，魔数都是对的。
func TestVerifyRejectsWrongProgram(t *testing.T) {
	dir := t.TempDir()
	path := realBinary(t, dir, "some-other-program 1.0")

	err := testTarget().Verify(context.Background(), path)
	if err == nil {
		t.Fatal("跑得起来但输出对不上的程序必须被拒绝")
	}
	if !strings.Contains(err.Error(), "对不上") {
		t.Errorf("错误消息应指向 --version 校验，实际：%v", err)
	}
}

func TestVerifyAcceptsCorrectBinary(t *testing.T) {
	dir := t.TempDir()
	path := realBinary(t, dir, "vps-agent 9.9.9")

	if err := testTarget().Verify(context.Background(), path); err != nil {
		t.Errorf("一个真的、输出正确的二进制应当通过，实际报错：%v", err)
	}
}

// shell 脚本能跑、输出也对，但不是 ELF —— 必须被第一关挡住。
// 这条用例的意义是确认两关是**都要过**，不是过一个就行。
func TestVerifyRequiresBothChecks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("自更新只在 Linux 上有意义")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake")
	os.WriteFile(path, []byte("#!/bin/sh\necho 'vps-agent 1.2.3'\n"), 0o755)

	if err := testTarget().Verify(context.Background(), path); err == nil {
		t.Error("shell 脚本虽然跑得起来、输出也对，但不是 ELF，应当被拒绝")
	}
}

func TestApplyKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	good := realBinary(t, dir, "vps-agent 9.9.9")
	newData, _ := os.ReadFile(good)

	exePath := filepath.Join(dir, "vps-agent")
	os.WriteFile(exePath, []byte("老版本"), 0o755)

	tgt := testTarget()
	tgt.BinaryPath = exePath
	res, err := tgt.Apply(context.Background(), bytes.NewReader(newData), nil)
	if err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	if res.BackupPath == "" {
		t.Fatal("应留下 .old 备份供回滚")
	}

	backup, err := os.ReadFile(exePath + ".old")
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if string(backup) != "老版本" {
		t.Errorf(".old 里应是旧二进制，实际 %q", backup)
	}
}

// 校验不过时，旧二进制必须原封不动。这是整个包最要紧的一条。
func TestApplyLeavesOldBinaryOnFailure(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "vps-agent")
	os.WriteFile(exePath, []byte("老版本"), 0o755)

	tgt := testTarget()
	tgt.BinaryPath = exePath
	_, err := tgt.Apply(context.Background(), strings.NewReader("<html>502</html>"), nil)
	if err == nil {
		t.Fatal("坏内容应当被拒绝")
	}

	got, _ := os.ReadFile(exePath)
	if string(got) != "老版本" {
		t.Errorf("校验失败后旧二进制被动过了：%q", got)
	}
	// 临时文件也不能留下。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".test-new-") {
			t.Errorf("失败后留下了临时文件 %s", e.Name())
		}
	}
}

// 升级不能让磁盘越用越多：.old 每次覆盖，始终只有一份。
func TestBackupNeverAccumulates(t *testing.T) {
	dir := t.TempDir()
	good := realBinary(t, dir, "vps-agent 9.9.9")
	newData, _ := os.ReadFile(good)
	os.Remove(good)
	os.Remove(filepath.Join(dir, "main.go"))

	exePath := filepath.Join(dir, "vps-agent")
	os.WriteFile(exePath, []byte("v1"), 0o755)

	tgt := testTarget()
	tgt.BinaryPath = exePath
	for range 4 {
		if _, err := tgt.Apply(context.Background(), bytes.NewReader(newData), nil); err != nil {
			t.Fatalf("替换失败: %v", err)
		}
	}

	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// 升了四次，目录里应该只剩当前二进制和一份 .old。
	if len(names) != 2 {
		t.Errorf("升级四次后目录里有 %d 个文件（应为 2 个：当前 + 一份 .old）：%v",
			len(names), names)
	}
}

// 下载到一半被杀（机器重启、OOM、systemd stop）会留下孤儿临时文件，
// 反复几次就是一串七八兆的碎片，而正常路径上没人会去收拾它们。
func TestCleanupRemovesOrphanTempFiles(t *testing.T) {
	dir := t.TempDir()

	orphans := []string{".test-new-123456", ".test-new-abcdef"}
	for _, name := range orphans {
		os.WriteFile(filepath.Join(dir, name), []byte("下了一半"), 0o644)
	}
	keep := []string{"vps-agent", "vps-agent.old", "别的程序"}
	for _, name := range keep {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755)
	}

	CleanupIn(dir, ".test-new-", nil)

	for _, name := range orphans {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("残留文件 %s 没被清掉", name)
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("不该动的文件 %s 被删了: %v", name, err)
		}
	}
}
