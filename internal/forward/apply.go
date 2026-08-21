package forward

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/zxcll/vps-panel/internal/resolver"
)

// nftRun 是执行 nft 的入口，测试里替换掉就能在没有 root 的机器上跑。
var nftRun = func(args []string, stdin string) (string, error) {
	cmd := exec.Command("nft", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Apply 用一次 nft 调用原子地替换整张表。
//
// 脚本的写法是「add table + delete table + 重新定义」：add 保证表一定存在
// （delete 一张不存在的表会报错），delete 清掉旧内容，然后整份新定义在同一个
// nft 事务里生效。中途出错的话 nft 会整体回滚，旧规则原封不动继续转发。
//
// 副作用：每次 apply 都会把所有 counter 清零。调用方必须在 apply 之前先采一次样
// 结转，否则这一段流量就丢了 —— 见 Dataplane.Reconcile。
func Apply(rules []Rule) error {
	var script strings.Builder
	script.WriteString(fmt.Sprintf("add table %s %s\n", TableFamily, TableName))
	script.WriteString(fmt.Sprintf("delete table %s %s\n", TableFamily, TableName))
	script.WriteString(RenderRuleset(rules))

	if _, err := nftRun([]string{"-f", "-"}, script.String()); err != nil {
		return fmt.Errorf("应用 nftables 规则失败: %w", err)
	}

	// IPv4 回环目标要开 route_localnet，否则内核会把「目标是 127.0.0.1 的
	// 非本机来包」当成火星包直接丢掉。IPv6 不需要，它走的是 redirect。
	for _, r := range rules {
		if r.DestIP != "" && IsLoopback(r.DestIP) && !IsIPv6(r.DestIP) {
			_ = os.WriteFile("/proc/sys/net/ipv4/conf/all/route_localnet", []byte("1\n"), 0o644)
			break
		}
	}
	return nil
}

// Flush 删掉整张表。探针退出时调用，免得留下没人管的规则。
func Flush() error {
	script := fmt.Sprintf("add table %s %s\ndelete table %s %s\n",
		TableFamily, TableName, TableFamily, TableName)
	if _, err := nftRun([]string{"-f", "-"}, script); err != nil {
		return fmt.Errorf("清理 nftables 规则失败: %w", err)
	}
	return nil
}

// Available 判断系统上有没有 nft 命令。
func Available() bool {
	_, err := exec.LookPath("nft")
	return err == nil
}

// Probe 实际跑一次 nft，确认它不仅存在而且能用
// （容器里常见的情况是二进制在但内核模块没加载）。
func Probe() error {
	if _, err := nftRun([]string{"list", "tables"}, ""); err != nil {
		return fmt.Errorf("nftables 不可用: %w", err)
	}
	return nil
}

func sysctlEnabled(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

// IPForwardEnabled 报告 IPv4 转发开关的状态。
func IPForwardEnabled() bool { return sysctlEnabled("/proc/sys/net/ipv4/ip_forward") }

// IPv6ForwardEnabled 报告 IPv6 转发开关的状态。
func IPv6ForwardEnabled() bool { return sysctlEnabled("/proc/sys/net/ipv6/conf/all/forwarding") }

// EnableIPForward 打开内核转发开关并写进 sysctl.d，重启后依然生效。
// 没开这个开关的话 DNAT 之后的包会被内核直接丢掉，现象是「规则看着对但就是不通」。
func EnableIPForward() error {
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1\n"), 0o644)
	body := "net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n"
	return os.WriteFile("/etc/sysctl.d/99-vps-forward.conf", []byte(body), 0o644)
}

// ResolveHosts 把规则里的域名目标解析成 IP，返回解析后的副本。
//
// 返回的 changed 表示至少有一条规则的 DestIP 变了 —— 只有变了才值得重新 apply，
// 否则每次刷新都白白清一次 counter。
//
// 解析失败的规则**保留原来的 DestIP**，并把错误汇总返回。DNS 抽风不该把
// 正在跑的连接拆掉；调用方拿到 err 的同时也拿到了可用的 out。
func ResolveHosts(ctx context.Context, rules []Rule, res *resolver.Resolver) (out []Rule, changed bool, err error) {
	out = make([]Rule, len(rules))
	copy(out, rules)

	var failures []string
	for i := range out {
		if out[i].DestHost == "" {
			continue
		}
		ip, lerr := res.LookupIPv4(ctx, out[i].DestHost)
		if lerr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", out[i].DestHost, lerr))
			continue
		}
		if ip != out[i].DestIP {
			out[i].DestIP = ip
			changed = true
		}
	}
	if len(failures) > 0 {
		return out, changed, fmt.Errorf("域名解析失败 —— %s", strings.Join(failures, "; "))
	}
	return out, changed, nil
}
