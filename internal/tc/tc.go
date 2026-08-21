// Package tc 用 tc HTB 给转发规则限速。
//
// 分类靠 nftables 打的 fwmark（见 internal/forward 的 Rule.ShapeMark），
// 这里只负责把 mark 映射到 HTB 的叶子类上。
//
// 已知限制：tc 只能限**出**方向。对中转机来说这其实够用 —— 转发的两个方向
// （去目标、回客户端）都是本机的出方向，所以一条规则的上下行都会落进同一个类，
// 共享同一份带宽。
package tc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Class 是一个 HTB 叶子：一个类加一条把 fwmark 喂给它的过滤器。
type Class struct {
	ClassID string // "1:<minor 十六进制>"
	Rate    string // tc 速率表达式，rate 和 ceil 取同一个值（硬上限，不借带宽）
	Handle  string // 过滤器匹配的 fwmark，"0x..."
}

// Shaped 是限速所需的最小信息，避免 tc 包反向依赖 forward 包。
type Shaped struct {
	Mark          uint32
	Minor         int // HTB 类的 minor 号，用监听端口，天然唯一
	BandwidthMbps int
}

// planClasses 从限速项推出 HTB 叶子列表。输出按 ClassID 排序，保证每次生成的
// 脚本完全一致，方便 diff 和排查。
func planClasses(items []Shaped) []Class {
	seen := map[int]bool{}
	out := make([]Class, 0, len(items))
	for _, it := range items {
		if it.Mark == 0 || it.BandwidthMbps <= 0 || seen[it.Minor] {
			continue
		}
		seen[it.Minor] = true
		out = append(out, Class{
			ClassID: fmt.Sprintf("1:%x", it.Minor),
			Rate:    fmt.Sprintf("%dmbit", it.BandwidthMbps),
			Handle:  fmt.Sprintf("0x%x", it.Mark),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClassID < out[j].ClassID })
	return out
}

// Apply 按 items 重建 iface 上的 HTB 树。
//
// 每次都是先整棵拆掉再重建，这样对账逻辑退化成「照着算一遍」，
// 不用去比对现有状态该增该删 —— 限速规则数量很少，重建的开销可以忽略。
//
// 树的形状：
//
//	qdisc 1: htb default 1
//	  class 1:1        默认类，超大 ceiling，没打 mark 的流量不受影响
//	  class 1:<minor>  每条限速规则一个类
//	filter fw <mark> → classid
func Apply(items []Shaped, iface string) error {
	if iface == "" {
		return nil
	}
	classes := planClasses(items)

	// 无条件先拆，保证状态确定。没有树的时候 del 会报错，忽略掉。
	_ = exec.Command("tc", "qdisc", "del", "dev", iface, "root").Run()
	if len(classes) == 0 {
		return nil
	}

	if err := run("tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", "1:1",
		"htb", "rate", "100gbit"); err != nil {
		return err
	}
	for _, c := range classes {
		if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", c.ClassID,
			"htb", "rate", c.Rate, "ceil", c.Rate); err != nil {
			return fmt.Errorf("建类 %s: %w", c.ClassID, err)
		}
		// IPv4 和 IPv6 各挂一条过滤器：fw 过滤器是按 protocol 注册的，
		// 只挂 ip 的话 IPv6 流量不会被分类，限速就形同虚设。
		for _, proto := range []string{"ip", "ipv6"} {
			if err := run("tc", "filter", "add", "dev", iface, "parent", "1:", "protocol", proto,
				"handle", c.Handle, "fw", "classid", c.ClassID); err != nil {
				return fmt.Errorf("建过滤器 %s/%s: %w", proto, c.Handle, err)
			}
		}
	}
	return nil
}

// Clear 拆掉整棵树。探针退出时调用。
func Clear(iface string) {
	if iface == "" {
		return
	}
	_ = exec.Command("tc", "qdisc", "del", "dev", iface, "root").Run()
}

// Available 判断系统上有没有 tc 命令。
func Available() bool {
	_, err := exec.LookPath("tc")
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DefaultIface 从 /proc/net/route 里找默认路由所在的网卡。
// 找不到返回空串，调用方回落到 --iface 参数或者干脆不限速。
func DefaultIface() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultIface(data)
}

// parseDefaultIface 从 /proc/net/route 内容里挑默认路由网卡。
//
// 机器上同时跑 Docker/libvirt 时可能有不止一条默认路由。优先挑物理网卡：
// 给容器网桥（docker0、br-xxx、veth*）限速是限不到真正的出口的。
// 虚拟网卡只作为兜底 —— 万一整台机器唯一的默认路由就是虚拟的，总比不限强。
func parseDefaultIface(data []byte) string {
	var fallback string
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		// 第二列是目标网络，全 0 即默认路由。
		if len(fields) < 2 || fields[1] != "00000000" {
			continue
		}
		iface := fields[0]
		if isVirtualIface(iface) {
			if fallback == "" {
				fallback = iface
			}
			continue
		}
		return iface
	}
	return fallback
}

// virtualIfacePrefixes 列的是容器/虚拟化栈加出来的网卡，不是真正的上联口。
// "br-" 只匹配 Docker 自建的网桥，不会误伤手工配的 br0。
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "vnet", "lo"}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
