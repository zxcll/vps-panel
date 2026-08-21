// Package collector 从 /proc 采集网卡流量和机器状态。
//
// 只读 /proc，不依赖任何外部命令，所以探针能跑在最精简的镜像里。
package collector

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot 是一次采集的结果。
type Snapshot struct {
	BootID string
	Iface  string
	// Rx/Tx 是网卡自开机以来的累计收发字节，不是增量。
	// 面板负责做差值——探针不记账，重启后也就无所谓丢不丢状态。
	Rx int64
	Tx int64

	Uptime     int64
	Load1      float64
	CPUPercent float64
	MemTotal   int64
	MemUsed    int64
	DiskTotal  int64
	DiskUsed   int64
}

// 这些网卡不参与统计：回环、容器网桥、虚拟网卡。
// 它们的流量要么是本机内部的，要么会和物理网卡重复计算。
var excludedPrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "vmbr",
	"tun", "tap", "wg", "kube", "cni", "flannel", "cali",
	"nerdctl", "podman", "dummy", "sit", "teql", "ifb", "gre",
}

func isExcluded(name string) bool {
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type Collector struct {
	// Iface 指定要统计的网卡。留空则自动识别默认路由所在的网卡。
	Iface string
	// AllIfaces 为 true 时汇总所有物理网卡（多网卡机器用）。
	AllIfaces bool

	once     sync.Once
	bootID   string
	lastCPU  cpuTimes
	hasLastC bool
}

func New(iface string, all bool) *Collector {
	return &Collector{Iface: iface, AllIfaces: all}
}

// BootID 读 /proc/sys/kernel/random/boot_id。这个值每次开机都会变，
// 面板靠它识别"网卡计数器已经归零了"。
func (c *Collector) BootID() string {
	c.once.Do(func() {
		// 允许显式覆盖。两个用处：
		//   1. 测试里模拟机器重启
		//   2. 容器里 /proc/sys/kernel/random/boot_id 是宿主机的，
		//      多个容器会撞成同一个值，需要各自指定
		if v := strings.TrimSpace(os.Getenv("VPS_AGENT_BOOT_ID")); v != "" {
			c.bootID = v
			return
		}

		b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err == nil {
			c.bootID = strings.TrimSpace(string(b))
		}
		if c.bootID == "" {
			// 读不到就退化成"进程启动时间"。精度差一些
			// （探针重启会被当成机器重启，导致少算一点流量），
			// 但比完全没有重启检测强。
			c.bootID = "fallback-" + strconv.FormatInt(time.Now().Unix(), 10)
		}
	})
	return c.bootID
}

// Collect 采集一次。
func (c *Collector) Collect() (Snapshot, error) {
	s := Snapshot{BootID: c.BootID()}

	stats, err := readNetDev()
	if err != nil {
		return s, err
	}

	switch {
	case c.Iface != "":
		st, ok := stats[c.Iface]
		if !ok {
			return s, fmt.Errorf("找不到网卡 %q，本机可用网卡: %s", c.Iface, strings.Join(names(stats), ", "))
		}
		s.Iface, s.Rx, s.Tx = c.Iface, st.rx, st.tx

	case c.AllIfaces:
		var names []string
		for name, st := range stats {
			if isExcluded(name) {
				continue
			}
			names = append(names, name)
			s.Rx += st.rx
			s.Tx += st.tx
		}
		if len(names) == 0 {
			return s, fmt.Errorf("没有找到任何可统计的物理网卡")
		}
		// 网卡名拼起来当标识。名字集合变了面板会当成"换网卡"重建基线，
		// 这正是我们要的行为：口径变了就不该继续做差。
		s.Iface = strings.Join(sorted(names), "+")

	default:
		iface, err := defaultIface(stats)
		if err != nil {
			return s, err
		}
		st := stats[iface]
		s.Iface, s.Rx, s.Tx = iface, st.rx, st.tx
	}

	// 下面这些采不到也不影响计费，失败就留零值
	s.Uptime, _ = readUptime()
	s.Load1, _ = readLoad1()
	s.MemTotal, s.MemUsed, _ = readMemory()
	s.DiskTotal, s.DiskUsed, _ = readDisk("/")
	s.CPUPercent = c.cpuPercent()

	return s, nil
}

type ifaceStat struct{ rx, tx int64 }

// readNetDev 解析 /proc/net/dev。
// 格式：接口名: 接收字节 接收包数 ... 发送字节 发送包数 ...
func readNetDev() (map[string]ifaceStat, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("读取 /proc/net/dev: %w", err)
	}
	defer f.Close()

	out := make(map[string]ifaceStat)
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 { // 前两行是表头
			continue
		}
		text := sc.Text()
		idx := strings.IndexByte(text, ':')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(text[:idx])
		fields := strings.Fields(text[idx+1:])
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseInt(fields[0], 10, 64)
		tx, err2 := strconv.ParseInt(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = ifaceStat{rx: rx, tx: tx}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("/proc/net/dev 里没有解析出任何网卡")
	}
	return out, nil
}

// defaultIface 找默认路由所在的网卡——那才是对外流量真正走的口子。
func defaultIface(stats map[string]ifaceStat) (string, error) {
	if name := routeDefaultIface(); name != "" {
		if _, ok := stats[name]; ok {
			return name, nil
		}
	}

	// 没有默认路由（少见）时，退而求其次：挑流量最大的物理网卡
	best, bestTotal := "", int64(-1)
	for name, st := range stats {
		if isExcluded(name) {
			continue
		}
		if total := st.rx + st.tx; total > bestTotal {
			best, bestTotal = name, total
		}
	}
	if best == "" {
		return "", fmt.Errorf("无法确定主网卡，请用 --iface 显式指定")
	}
	return best, nil
}

// routeDefaultIface 从 /proc/net/route 里找目标地址为 0.0.0.0 的那条路由。
func routeDefaultIface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		// fields[1] 是十六进制的目标地址，00000000 表示默认路由
		if fields[1] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

func names(stats map[string]ifaceStat) []string {
	out := make([]string, 0, len(stats))
	for k := range stats {
		out = append(out, k)
	}
	return sorted(out)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func readUptime() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/uptime 格式异常")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}

func readLoad1() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/loadavg 格式异常")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// readMemory 返回总内存和已用内存（字节）。
// 已用 = 总量 - MemAvailable，比 MemFree 更贴近"实际可用"。
func readMemory() (total, used int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var available int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		v *= 1024 // /proc/meminfo 的单位是 kB
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("无法读取内存总量")
	}
	used = total - available
	if used < 0 {
		used = 0
	}
	return total, used, nil
}

type cpuTimes struct {
	idle  int64
	total int64
	valid bool
}

// cpuPercent 用两次采样之间的差值算 CPU 占用。
// 第一次调用没有基准，返回 0。
func (c *Collector) cpuPercent() float64 {
	cur, err := readCPUTimes()
	if err != nil {
		return 0
	}
	prev := c.lastCPU
	c.lastCPU = cur

	if !prev.valid {
		return 0
	}
	dTotal := cur.total - prev.total
	dIdle := cur.idle - prev.idle
	if dTotal <= 0 {
		return 0
	}
	pct := float64(dTotal-dIdle) / float64(dTotal) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}, fmt.Errorf("/proc/stat 为空")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, fmt.Errorf("/proc/stat 首行格式异常")
	}

	var t cpuTimes
	for i, f := range fields[1:] {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			continue
		}
		t.total += v
		// 第 4、5 个字段分别是 idle 和 iowait，都算作空闲
		if i == 3 || i == 4 {
			t.idle += v
		}
	}
	t.valid = true
	return t, nil
}
