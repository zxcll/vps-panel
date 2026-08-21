package forward

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// 防火墙垫片：往其他工具预留的"用户扩展链"里插放行规则。
//
// 为什么需要：装了 Docker 或 ufw 的机器上，FORWARD 链的默认策略常常是 drop。
// 我们的 nftables 表只做 DNAT 和计数，不碰 filter 的放行决策，
// 所以在这种机器上包会 DNAT 成功然后被别人的 FORWARD 规则丢掉，
// 现象是「nft 里规则都在、就是不通」，非常难查。
//
// 每个垫片都是尽力而为：失败只记日志，绝不阻断核心表的 apply。
// 插进去的规则统一打上 ownerComment，Cleanup 时按这个注释精确删除，
// 不会误伤别人的规则。

// ownerComment 是我们插到别人链里的每条规则的标记。
const ownerComment = "vps-panel forward managed"

// firewallShim 是一种防火墙工具的集成。
type firewallShim interface {
	// Name 是日志里用的短名。
	Name() string
	// Detect 报告目标链此刻存不存在。要够便宜，每次 Sync 都会调。
	Detect() bool
	// Sync 让目标链反映当前状态：先删掉自己上次插的，再插当前需要的。幂等。
	Sync(kernelRules []Rule, listenPorts []int) error
	// Cleanup 删掉自己插过的所有规则。幂等。
	Cleanup() error
}

// firewallSet 是内置垫片的集合，按注册顺序依次驱动。
type firewallSet struct {
	shims []firewallShim
}

func defaultFirewallSet() *firewallSet {
	return &firewallSet{shims: []firewallShim{newDockerUserShim(), newUfwShim()}}
}

// Sync 驱动所有已检测到的垫片。单个垫片失败不影响其他的，错误汇总返回。
func (f *firewallSet) Sync(kernelRules []Rule, listenPorts []int) error {
	var errs []string
	for _, s := range f.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Sync(kernelRules, listenPorts); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (f *firewallSet) Cleanup() error {
	var errs []string
	for _, s := range f.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Cleanup(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// DetectedNames 返回此刻检测到的垫片名，探针启动时打日志用。
func (f *firewallSet) DetectedNames() []string {
	var out []string
	for _, s := range f.shims {
		if s.Detect() {
			out = append(out, s.Name())
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Docker：DOCKER-USER 链
// ---------------------------------------------------------------------------

// dockerUserShim 往 Docker 的 DOCKER-USER 链插放行。
// Docker 把这条链放在 FORWARD 的最前面，就是留给外部程序追加规则用的，
// 不会和 Docker 自己生成的规则冲突。
type dockerUserShim struct {
	run    func(args ...string) (string, error)
	script func(s string) error
}

func newDockerUserShim() *dockerUserShim {
	return &dockerUserShim{run: runNftCapture, script: runNftScript}
}

func (s *dockerUserShim) Name() string { return "docker-user" }

func (s *dockerUserShim) Detect() bool {
	_, err := s.run("list", "chain", "ip", "filter", "DOCKER-USER")
	return err == nil
}

// Sync 同时处理 ip 和 ip6 两个族。Docker 在每个族里各建一条 DOCKER-USER，
// 往其中一条加规则不会覆盖另一条。
// Docker 不管 INPUT 过滤，所以用户态监听端口这里不处理。
func (s *dockerUserShim) Sync(kernelRules []Rule, _ []int) error {
	for _, family := range []string{"ip", "ip6"} {
		if err := s.syncFamily(family, kernelRules); err != nil {
			return err
		}
	}
	return nil
}

func (s *dockerUserShim) syncFamily(family string, rules []Rule) error {
	out, err := s.run("-a", "list", "chain", family, "filter", "DOCKER-USER")
	if err != nil {
		return nil // 这个族里没有这条链，没什么好做的
	}

	var b strings.Builder
	for _, h := range parseHandles(out) {
		fmt.Fprintf(&b, "delete rule %s filter DOCKER-USER handle %d\n", family, h)
	}
	// 先放行已建立连接的回程，再逐条放行 DNAT 目标。
	// 就算一条规则都没有也要写这一句，将来加规则时回程才有路可走。
	fmt.Fprintf(&b, "add rule %s filter DOCKER-USER ct state established,related counter accept comment %q\n",
		family, ownerComment)

	want6 := family == "ip6"
	for _, r := range rules {
		// 地址族必须和链的族对上：往 ip6 链里写 IPv4 字面量，nft 会在
		// 解析阶段就报错，整个脚本（含其他合法规则）一起失败。
		if r.DestIP == "" || IsIPv6(r.DestIP) != want6 {
			continue
		}
		fmt.Fprintf(&b, "add rule %s filter DOCKER-USER %s daddr %s %s counter accept comment %q\n",
			family, family, r.DestIP, protoDportMatch(r.Proto, r.DestPort), ownerComment)
	}
	return s.script(b.String())
}

func (s *dockerUserShim) Cleanup() error {
	var errs []string
	for _, family := range []string{"ip", "ip6"} {
		// 两个族都要试完：在 ip 这一轮提前返回的话，ip6 链里我们插的规则
		// 就再也没有进程会去清理了。
		if err := cleanupNftChain(s.run, s.script, family, "filter", "DOCKER-USER"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// ufw：ufw-user-forward / ufw-user-input 链
// ---------------------------------------------------------------------------

// ufwShim 往 ufw 预留的用户扩展链插放行：
// ufw-user-forward 放行 DNAT 目标，ufw-user-input 放行用户态监听端口
// （INPUT 默认 drop 时，内嵌 relay 的监听端口外面根本连不上）。
//
// 全部改动走 iptables/ip6tables 而不是裸 nft：ufw 用的是 iptables-nft，
// 直接用 nft 写它的链会破坏 iptables 的代数跟踪，ufw status 就不认了。
type ufwShim struct {
	run4 func(args ...string) (string, error)
	run6 func(args ...string) (string, error)
}

func newUfwShim() *ufwShim {
	return &ufwShim{
		run4: func(args ...string) (string, error) { return runCapture("iptables", args...) },
		run6: func(args ...string) (string, error) { return runCapture("ip6tables", args...) },
	}
}

func (s *ufwShim) Name() string { return "ufw" }

func (s *ufwShim) Detect() bool {
	_, err := s.run4("-L", "ufw-user-forward", "-n")
	return err == nil
}

func (s *ufwShim) Sync(kernelRules []Rule, listenPorts []int) error {
	if err := s.syncForward(s.run4, "ufw-user-forward", kernelRules, false); err != nil {
		return err
	}
	if err := s.syncForward(s.run6, "ufw6-user-forward", kernelRules, true); err != nil {
		return err
	}
	if err := s.syncInput(s.run4, "ufw-user-input", listenPorts); err != nil {
		return err
	}
	return s.syncInput(s.run6, "ufw6-user-input", listenPorts)
}

func (s *ufwShim) syncForward(run func(...string) (string, error), chain string, rules []Rule, want6 bool) error {
	if _, err := run("-L", chain, "-n"); err != nil {
		return nil // 这个族里没这条链
	}
	if err := deleteOwnedIpt(run, chain); err != nil {
		return err
	}
	for _, r := range rules {
		if r.DestIP == "" || IsIPv6(r.DestIP) != want6 {
			continue
		}
		for _, p := range splitProtos(r.Proto) {
			args := []string{"-A", chain, "-d", r.DestIP, "-p", p,
				"--dport", strconv.Itoa(r.DestPort), "-j", "ACCEPT",
				"-m", "comment", "--comment", ownerComment}
			if _, err := run(args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ufwShim) syncInput(run func(...string) (string, error), chain string, ports []int) error {
	if _, err := run("-L", chain, "-n"); err != nil {
		return nil
	}
	if err := deleteOwnedIpt(run, chain); err != nil {
		return err
	}
	for _, port := range ports {
		args := []string{"-A", chain, "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT",
			"-m", "comment", "--comment", ownerComment}
		if _, err := run(args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *ufwShim) Cleanup() error {
	var errs []string
	for _, pair := range []struct {
		run   func(...string) (string, error)
		chain string
	}{
		{s.run4, "ufw-user-forward"}, {s.run4, "ufw-user-input"},
		{s.run6, "ufw6-user-forward"}, {s.run6, "ufw6-user-input"},
	} {
		if _, err := pair.run("-L", pair.chain, "-n"); err != nil {
			continue
		}
		if err := deleteOwnedIpt(pair.run, pair.chain); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// deleteOwnedIpt 反复删除链里带我们注释的第一条规则，直到删不动为止。
//
// 之所以循环「查行号再删」而不是一次算出所有行号批量删：删掉一条之后
// 后面所有规则的行号都会往前挪，一次性算好的行号立刻就失效了。
// 每轮重新查是最笨但最不会错的写法，而且规则数量很少。
func deleteOwnedIpt(run func(...string) (string, error), chain string) error {
	for range 256 { // 上限纯粹是防死循环，正常情况下几轮就删完了
		out, err := run("-L", chain, "-n", "--line-numbers")
		if err != nil {
			return nil
		}
		line := firstOwnedLine(out)
		if line == 0 {
			return nil
		}
		if _, err := run("-D", chain, strconv.Itoa(line)); err != nil {
			return err
		}
	}
	return nil
}

// firstOwnedLine 从 iptables -L --line-numbers 的输出里找第一条带我们注释的行号。
func firstOwnedLine(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, ownerComment) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.Atoi(fields[0]); err == nil {
			return n
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 公用小工具
// ---------------------------------------------------------------------------

// handleRegex 抓 `nft -a list chain` 每行末尾的 `# handle N`。
var handleRegex = regexp.MustCompile(`#\s*handle\s+(\d+)\s*$`)

// parseHandles 从 nft -a 的输出里挑出所有带我们注释的规则句柄。
// 没有注释的行（别人的规则）一律跳过。
func parseHandles(listOutput string) []int {
	var out []int
	for _, line := range strings.Split(listOutput, "\n") {
		if !strings.Contains(line, `comment "`+ownerComment+`"`) {
			continue
		}
		m := handleRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// cleanupNftChain 删掉某条 nft 链里所有带我们注释的规则。链不存在或已经干净时返回 nil。
func cleanupNftChain(run func(...string) (string, error), script func(string) error, family, table, chain string) error {
	out, err := run("-a", "list", "chain", family, table, chain)
	if err != nil {
		return nil
	}
	stale := parseHandles(out)
	if len(stale) == 0 {
		return nil
	}
	var b strings.Builder
	for _, h := range stale {
		fmt.Fprintf(&b, "delete rule %s %s %s handle %d\n", family, table, chain, h)
	}
	return script(b.String())
}

func runNftCapture(args ...string) (string, error) { return runCapture("nft", args...) }

func runNftScript(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	_, err := nftRun([]string{"-f", "-"}, s)
	return err
}

func runCapture(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %v: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
