package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/protocol"
	"github.com/zxcll/vps-panel/internal/resolver"
)

// forwardStateVersion 是状态文件的格式版本。
// 版本对不上时按"没有状态"处理：宁可重新拿一次面板下发的规则，
// 也不要拿一份读歪了的数据去操作防火墙。
const forwardStateVersion = 1

// dnsRefreshInterval 是目标域名的重解析周期。
// 只有解析结果真的变了才会重新 apply —— apply 会清零 nft 计数，
// 无谓的重试会让计数在面板上看起来一跳一跳的。
const dnsRefreshInterval = 60 * time.Second

// probeTimeout 是连通性测试的拨号上限。给得比较短：这是人点了「测试」在等结果，
// 拨不通的话快点告诉他比精确区分"慢"和"不通"更重要。
const probeTimeout = 5 * time.Second

// forwardState 是要跨进程重启保留的转发状态。
type forwardState struct {
	Version int `json:"version"`

	// Epoch 标识"这一份计数是从哪次开始攒的"。状态文件丢了就换一个新值，
	// 面板看到 Epoch 变化就知道计数器归零了，把本次读数整个计入 ——
	// 和网卡计数器那条链路上 boot_id 的作用完全一样。
	Epoch string `json:"epoch"`

	// Rev 是面板给当前这版规则集的编号，重连后可以告诉面板本机是哪一版。
	Rev string `json:"rev"`

	// Rules 是面板下发的原始规则（DestIP 可能是上次的解析结果）。
	// 探针重启后照着它把转发恢复起来，不用等面板重新下发。
	Rules []forward.Rule `json:"rules"`

	Counters forward.CounterState `json:"counters"`
}

// forwarder 管理本机的端口转发：接收面板下发的规则集、落到数据面、
// 定期重解析域名、把计数汇总成上报用的样本。
type forwarder struct {
	dp        *forward.Dataplane
	res       *resolver.Resolver
	log       *slog.Logger
	statePath string

	mu    sync.Mutex
	epoch string
	rev   string
	// rules 是面板下发的原始规则，域名解析结果会写回它的 DestIP。
	rules []forward.Rule
}

type forwarderConfig struct {
	StatePath string
	Iface     string
	PoolSize  int
	Log       *slog.Logger
}

// newForwarder 建转发管理器并从磁盘恢复状态。
// 状态文件缺失或读不动都不算错误：换一个新 Epoch 从头开始就是了。
func newForwarder(cfg forwarderConfig) *forwarder {
	f := &forwarder{
		dp: forward.New(forward.Config{
			Iface:    cfg.Iface,
			PoolSize: cfg.PoolSize,
			Log:      cfg.Log,
		}),
		res:       resolver.New(),
		log:       cfg.Log,
		statePath: cfg.StatePath,
	}

	st, err := loadForwardState(cfg.StatePath)
	switch {
	case err != nil:
		f.log.Warn("转发状态文件读取失败，计数将从头开始",
			"路径", cfg.StatePath, "错误", err)
		f.epoch = newEpoch()
	case st == nil:
		f.epoch = newEpoch()
	default:
		f.epoch = st.Epoch
		f.rev = st.Rev
		f.rules = st.Rules
		f.dp.SeedCounters(st.Counters)
	}
	if f.epoch == "" {
		f.epoch = newEpoch()
	}
	return f
}

// Start 恢复上次的规则并起一个域名刷新循环。
//
// 恢复要尽早做：探针重启期间内核里的 nftables 表其实还在转发，
// 但用户态监听器是随进程没的，早一秒恢复就少一秒不通。
func (f *forwarder) Start(ctx context.Context) {
	f.mu.Lock()
	has := len(f.rules) > 0
	f.mu.Unlock()

	if has {
		if err := f.reconcile(ctx); err != nil {
			f.log.Error("恢复上次的转发规则失败", "错误", err)
		} else {
			f.log.Info("已恢复上次的转发规则", "条数", len(f.rules), "版本", f.rev)
		}
	}
	if names := f.dp.DetectedFirewalls(); len(names) > 0 {
		f.log.Info("检测到本机防火墙，已自动插入放行规则", "工具", names)
	}

	go f.refreshLoop(ctx)
}

// Apply 处理面板下发的一份完整规则集，返回回执。
func (f *forwarder) Apply(ctx context.Context, rs protocol.ApplyRuleset) protocol.ApplyAck {
	ack := protocol.ApplyAck{Rev: rs.Rev}

	for _, r := range rs.Rules {
		if err := forward.Validate(r); err != nil {
			ack.Error = fmt.Sprintf("规则 #%d 不合法: %v", r.HopID, err)
			return ack
		}
	}

	f.mu.Lock()
	// 保留上一版已经解析出来的 IP：面板只知道域名，重新下发时 DestIP 是空的。
	// 直接用空 IP 去 apply 会让这条规则短暂不通，白白断一次连接。
	prev := make(map[int64]string, len(f.rules))
	for _, r := range f.rules {
		if r.DestHost != "" && r.DestIP != "" {
			prev[r.HopID] = r.DestIP
		}
	}
	rules := make([]forward.Rule, len(rs.Rules))
	copy(rules, rs.Rules)
	for i := range rules {
		if rules[i].DestHost != "" && rules[i].DestIP == "" {
			rules[i].DestIP = prev[rules[i].HopID]
		}
	}
	f.rules = rules
	f.rev = rs.Rev
	f.mu.Unlock()

	if err := f.reconcile(ctx); err != nil {
		ack.Error = err.Error()
		return ack
	}

	ack.OK = true
	ack.Warning = f.warnings()
	f.log.Info("转发规则已生效", "条数", len(rules), "版本", rs.Rev)
	return ack
}

// reconcile 解析域名、落到数据面、把状态写回磁盘。
func (f *forwarder) reconcile(ctx context.Context) error {
	f.mu.Lock()
	rules := append([]forward.Rule(nil), f.rules...)
	f.mu.Unlock()

	resolved, _, err := forward.ResolveHosts(ctx, rules, f.res)
	if err != nil {
		// 解析失败的规则会保留上一次的 IP，所以这里只是提醒，不中断。
		f.log.Warn("部分目标域名解析失败，这些规则暂时沿用上次的解析结果", "错误", err)
	}

	if err := f.dp.Reconcile(resolved); err != nil {
		return err
	}

	f.mu.Lock()
	f.rules = resolved
	f.mu.Unlock()

	f.save()
	return nil
}

// refreshLoop 定期重解析目标域名，解析结果变了才重新下发。
func (f *forwarder) refreshLoop(ctx context.Context) {
	t := time.NewTicker(dnsRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.refreshOnce(ctx)
		}
	}
}

func (f *forwarder) refreshOnce(ctx context.Context) {
	f.mu.Lock()
	rules := append([]forward.Rule(nil), f.rules...)
	f.mu.Unlock()

	hasHost := false
	for _, r := range rules {
		if r.DestHost != "" {
			hasHost = true
			break
		}
	}
	if !hasHost {
		return
	}

	resolved, changed, err := forward.ResolveHosts(ctx, rules, f.res)
	if err != nil {
		f.log.Warn("刷新目标域名失败", "错误", err)
	}
	if !changed {
		return
	}

	f.mu.Lock()
	f.rules = resolved
	f.mu.Unlock()

	if err := f.dp.Reconcile(resolved); err != nil {
		f.log.Error("目标域名变化后重新下发失败", "错误", err)
		return
	}
	f.save()
	f.log.Info("目标域名解析结果变化，转发规则已更新")
}

// Samples 采一次计数，转成上报用的样本，顺手把状态落盘。
//
// 落盘放在这里是因为它和上报同频（默认 30 秒），而且刚采完样是
// 状态最新的时刻：进程被 kill -9 时磁盘上留的就是最近一次上报的水位。
func (f *forwarder) Samples() []protocol.ForwardSample {
	f.mu.Lock()
	epoch := f.epoch
	idle := len(f.rules) == 0
	f.mu.Unlock()

	// 一条规则都没配过：不采样也不落盘。探针默认开着转发功能，
	// 绝大多数机器上不会用到，不该为此每个上报周期都写一次磁盘。
	// 有过规则但刚被撤空的机器不算 idle，还要把最后一段计数报回去。
	if idle && len(f.dp.CounterState().Logical) == 0 {
		return nil
	}

	counters := f.dp.Counters()

	out := make([]protocol.ForwardSample, 0, len(counters))
	for _, c := range counters {
		out = append(out, protocol.ForwardSample{
			HopID:     c.HopID,
			Epoch:     epoch,
			BytesUp:   c.BytesUp,
			BytesDown: c.BytesDown,
		})
	}
	f.save()
	return out
}

// Probe 拨一次目标地址，并汇报本机的转发能力体检结果。
//
// 目标为空时只做体检不拨号 —— 面板测最后一跳到落地目标的连通性时会传地址，
// 而单纯想看看某台机器转发能力是否正常时不需要拨。
func (f *forwarder) Probe(ctx context.Context, target string) protocol.ForwardProbe {
	f.mu.Lock()
	rules := append([]forward.Rule(nil), f.rules...)
	f.mu.Unlock()

	p := protocol.ForwardProbe{
		Target:       target,
		NftAvailable: forward.Available(),
		IPForward:    forward.IPForwardEnabled(),
		Firewalls:    f.dp.DetectedFirewalls(),
		RuleCount:    len(rules),
	}

	if target == "" {
		p.OK = true
		return p
	}

	start := time.Now()
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	p.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		p.Error = err.Error()
		return p
	}
	conn.Close()
	p.OK = true
	return p
}

// ProbeListen 判断本机某个端口上有没有监听进程。
// 用户态转发要靠监听进程接活，没有就是没生效；内核态不需要监听，所以这项为假不算问题。
func (f *forwarder) ProbeListen(port int) bool {
	// 直接试着占一下这个端口：占得住说明没人在听。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// warnings 返回"规则生效了但有隐患"的提示，回执里带给面板。
func (f *forwarder) warnings() string {
	f.mu.Lock()
	rules := f.rules
	f.mu.Unlock()

	needKernel := false
	for _, r := range rules {
		if r.EffectiveMode() == forward.ModeKernel {
			needKernel = true
			break
		}
	}
	if !needKernel {
		return ""
	}
	if !forward.Available() {
		return "本机没有 nftables，内核态转发不会生效；请安装 nftables 包"
	}
	if !forward.IPForwardEnabled() {
		return "本机 ip_forward 未开启，内核态转发不会生效"
	}
	return ""
}

// Close 拆掉所有转发并把最后的计数落盘。
func (f *forwarder) Close() {
	// 先采一次样：这一段流量还没上报过，至少要留在磁盘上，
	// 下次启动时能连同 Epoch 一起接上。
	_ = f.dp.Counters()
	f.save()
	f.dp.Close()
}

// --- 状态文件 ---

func (f *forwarder) save() {
	f.mu.Lock()
	st := forwardState{
		Version:  forwardStateVersion,
		Epoch:    f.epoch,
		Rev:      f.rev,
		Rules:    f.rules,
		Counters: f.dp.CounterState(),
	}
	path := f.statePath
	f.mu.Unlock()

	if err := saveForwardState(path, st); err != nil {
		f.log.Warn("转发状态落盘失败，探针重启后计数会重新开始", "路径", path, "错误", err)
	}
}

// loadForwardState 读状态文件。文件不存在返回 (nil, nil)。
func loadForwardState(path string) (*forwardState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st forwardState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Version != forwardStateVersion {
		return nil, fmt.Errorf("状态文件版本为 %d，本探针只认 %d", st.Version, forwardStateVersion)
	}
	return &st, nil
}

// saveForwardState 原子地写状态文件：先写临时文件再 rename。
// 直接覆写的话，掉电正好卡在中间会留下半个 JSON，下次启动就读不出来了。
func saveForwardState(path string, st forwardState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newEpoch() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败在 Linux 上基本不可能。真发生了就用时间兜底 ——
		// Epoch 只要"每次重来都不同"，不需要不可预测。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
