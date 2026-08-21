package forward

import (
	"log/slog"
	"sync"
	"time"
)

// stalePruneAfter 是一跳从规则集里消失后，它的计数还保留多久。
//
// 不能一消失就删：Reconcile 会在删表前先采一次样把最后一段流量结转进来，
// 但那时候还没轮到上报。留一段时间等它被报上去，面板对认不出的 hop_id 会直接忽略，
// 所以多报几轮没有副作用。
const stalePruneAfter = 10 * time.Minute

// CounterState 是需要跨进程重启保留的计数状态。
//
// 两张表都要存：
//   - Logical 是对外的单调累计量，丢了面板就会当成"计数器归零"从头算
//   - LastRaw 是上次从后端读到的原始值。存下来的话，探针重启期间
//     nftables 表如果还在（规则没动过），那段流量还能靠差值补回来
type CounterState struct {
	// Logical[hopID] = [上行, 下行]，单调递增。
	Logical map[int64][2]int64 `json:"logical"`
	// LastRaw[hopID] = [上行, 下行]，后端的原始读数，会归零。
	LastRaw map[int64][2]int64 `json:"last_raw"`
}

// Dataplane 把内核态和用户态两个后端、以及防火墙垫片收拢到一组
// Reconcile / Counters / Close 接口后面。
//
// 它同时负责把两个后端会归零的原始计数换算成**单调递增**的逻辑累计量：
// nftables 每次 apply 都是删表重建，计数全部清零，用户态监听器重建时也一样。
// 上层（探针）只管把逻辑累计量原样报给面板，面板做差值 —— 和网卡计数器
// 那条链路的分工完全一致。
type Dataplane struct {
	kernel    kernelReconciler
	userspace *userspaceBackend
	fw        *firewallSet
	log       *slog.Logger
	now       func() time.Time

	mu sync.Mutex
	// lastUserspace 是回滚锚点。nft 本身是原子的不需要回滚，但内核这步失败时
	// 用户态已经先改完了，会停在一个比逻辑状态更靠前的位置 —— 而域名刷新
	// 只在解析结果真的变了才重新 apply，不会自动纠正回来。
	lastUserspace []Rule

	logical     map[int64][2]int64
	lastRaw     map[int64][2]int64
	absentSince map[int64]time.Time
}

// Config 是构造 Dataplane 的参数。
type Config struct {
	// Iface 是限速要挂的网卡，空串表示不限速。
	Iface string
	// PoolSize 是用户态每个端口的预连接数，0 关闭预连接。
	PoolSize int
	Log      *slog.Logger
}

func New(cfg Config) *Dataplane {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Dataplane{
		kernel:      &kernelBackend{iface: cfg.Iface, log: log},
		userspace:   newUserspaceBackend(cfg.PoolSize),
		fw:          defaultFirewallSet(),
		log:         log,
		now:         func() time.Time { return time.Now() },
		logical:     map[int64][2]int64{},
		lastRaw:     map[int64][2]int64{},
		absentSince: map[int64]time.Time{},
	}
}

// SeedCounters 用持久化下来的状态初始化计数。必须在第一次 Reconcile 之前调用。
func (d *Dataplane) SeedCounters(st CounterState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range st.Logical {
		d.logical[k] = v
	}
	for k, v := range st.LastRaw {
		d.lastRaw[k] = v
	}
}

// CounterState 导出当前计数状态，供上层落盘。
func (d *Dataplane) CounterState() CounterState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return CounterState{
		Logical: copyCounterMap(d.logical),
		LastRaw: copyCounterMap(d.lastRaw),
	}
}

// Reconcile 把规则集落到两个后端上。
//
// 第一步一定是采样：nft.Apply 会把 counter 全部清零，不先把这一段结转进
// 逻辑累计量，从上次采样到现在的流量就永远丢了。
//
// 之后的顺序是「用户态 → 内核态 → 防火墙垫片」。内核态硬失败时把用户态
// 回滚到上一份好的规则集，避免两边状态劈叉。垫片失败只记日志。
func (d *Dataplane) Reconcile(rules []Rule) error {
	d.sample()

	kernelRules, userspaceRules, err := Partition(rules)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.userspace.Reconcile(userspaceRules); err != nil {
		return err
	}
	if err := d.kernel.Reconcile(kernelRules); err != nil {
		if rbErr := d.userspace.Reconcile(d.lastUserspace); rbErr != nil {
			d.log.Error("内核规则应用失败后回滚用户态也失败", "错误", rbErr)
		}
		return err
	}
	if err := d.fw.Sync(kernelRules, d.userspace.ListenPorts()); err != nil {
		d.log.Warn("防火墙放行规则同步失败，转发可能被本机防火墙拦截", "错误", err)
	}
	d.lastUserspace = append([]Rule(nil), userspaceRules...)

	// 标记这一轮不在规则集里的跳，到期后清掉它们的计数。
	live := make(map[int64]bool, len(rules))
	for _, r := range rules {
		live[r.HopID] = true
	}
	d.markAbsentLocked(live)
	return nil
}

// Counters 先采一次样，再返回所有跳的单调累计计数。
func (d *Dataplane) Counters() []Counter {
	d.sample()

	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Counter, 0, len(d.logical))
	for hopID, v := range d.logical {
		out = append(out, Counter{HopID: hopID, BytesUp: v[0], BytesDown: v[1]})
	}
	return out
}

// sample 读一次两个后端的原始计数，折进逻辑累计量。
//
// 折算规则和面板处理网卡计数器的 ComputeDelta 是同一套：
// 原始值比上次小说明清零过，本次读数整个计入；否则算差值。
func (d *Dataplane) sample() {
	raw := make(map[int64][2]int64)

	kc, err := d.kernel.Counters()
	if err != nil {
		// 读不到内核计数（nft 不可用、表被别人删了）不该影响用户态的计数，
		// 更不该让整个采样失败。记一笔继续走。
		d.log.Warn("读取内核转发计数失败", "错误", err)
	}
	for _, c := range kc {
		v := raw[c.HopID]
		raw[c.HopID] = [2]int64{v[0] + c.BytesUp, v[1] + c.BytesDown}
	}
	for _, c := range d.userspace.Counters() {
		v := raw[c.HopID]
		raw[c.HopID] = [2]int64{v[0] + c.BytesUp, v[1] + c.BytesDown}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for hopID, cur := range raw {
		last := d.lastRaw[hopID]
		lg := d.logical[hopID]
		lg[0] += foldDelta(last[0], cur[0])
		lg[1] += foldDelta(last[1], cur[1])
		d.logical[hopID] = lg
		d.lastRaw[hopID] = cur
	}
	d.pruneStaleLocked()
}

// foldDelta 把一次原始读数换算成增量。
// cur < last 意味着计数器被清零过（apply 删表重建、监听器重建、进程重启），
// 这时候 cur 本身就是清零之后累积的全部量。
func foldDelta(last, cur int64) int64 {
	if cur < 0 {
		return 0
	}
	if cur < last {
		return cur
	}
	return cur - last
}

// markAbsentLocked 记录哪些跳这一轮不在规则集里了。重新出现时清掉标记。
func (d *Dataplane) markAbsentLocked(live map[int64]bool) {
	now := d.now()
	for hopID := range d.logical {
		if live[hopID] {
			delete(d.absentSince, hopID)
			continue
		}
		if _, ok := d.absentSince[hopID]; !ok {
			d.absentSince[hopID] = now
		}
	}
}

// pruneStaleLocked 清掉消失够久的跳的计数，免得长期运行的探针无限攒 map。
func (d *Dataplane) pruneStaleLocked() {
	if len(d.absentSince) == 0 {
		return
	}
	now := d.now()
	for hopID, since := range d.absentSince {
		if now.Sub(since) < stalePruneAfter {
			continue
		}
		delete(d.logical, hopID)
		delete(d.lastRaw, hopID)
		delete(d.absentSince, hopID)
	}
}

// Close 拆掉所有监听、清空 nftables 表和 tc 树、撤掉防火墙垫片规则。
func (d *Dataplane) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.userspace.Close()
	d.kernel.Close()
	if err := d.fw.Cleanup(); err != nil {
		d.log.Warn("清理防火墙放行规则失败", "错误", err)
	}
}

// DetectedFirewalls 返回此刻检测到的防火墙工具名，探针启动时打日志用。
func (d *Dataplane) DetectedFirewalls() []string { return d.fw.DetectedNames() }

func copyCounterMap(m map[int64][2]int64) map[int64][2]int64 {
	out := make(map[int64][2]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
