package forward

import (
	"log/slog"
	"strconv"
	"sync"

	"github.com/zxcll/vps-panel/internal/tc"
)

// kernelReconciler 是内核后端的接缝。真实实现要 root 才能跑，
// 所以 Dataplane 的测试从这里注入一个假的。
type kernelReconciler interface {
	Reconcile(rules []Rule) error
	Counters() ([]Counter, error)
	Close()
}

type kernelBackend struct {
	iface string
	log   *slog.Logger

	// mu 保护 applied：Counters 要靠它把 (协议, 端口) 映射回 HopID。
	mu      sync.Mutex
	applied []Rule
}

// Reconcile 先原子替换 nftables 规则，再重建 tc 树。
//
// 顺序有讲究：nft 失败时旧表原样保留，所以先做它；tc 放后面，避免出现
// 「限速类已经指向一条还没生效的规则」的中间态。
//
// tc 失败只记日志不往上抛 —— 限速没了顶多跑满带宽，比整条转发不通要好。
func (k *kernelBackend) Reconcile(rules []Rule) error {
	if err := Apply(rules); err != nil {
		return err
	}

	shaped := make([]tc.Shaped, 0, len(rules))
	for _, r := range rules {
		if m := r.ShapeMark(); m != 0 {
			shaped = append(shaped, tc.Shaped{Mark: m, Minor: r.ListenPort, BandwidthMbps: r.BandwidthMbps})
		}
	}
	if err := tc.Apply(shaped, k.iface); err != nil {
		k.log.Warn("限速配置失败，转发不受影响", "错误", err)
	}

	k.mu.Lock()
	k.applied = append([]Rule(nil), rules...)
	k.mu.Unlock()
	return nil
}

// Counters 读回 nft 计数并归并到 HopID 上。
//
// 一条 tcp+udp 规则在 nft 里是两组 counter（tcp 一组、udp 一组），
// 这里按 HopID 相加合成一条 —— 面板看到的是「这一跳用了多少」，
// 不需要再按协议拆开。
func (k *kernelBackend) Counters() ([]Counter, error) {
	k.mu.Lock()
	noRules := len(k.applied) == 0
	k.mu.Unlock()
	if noRules {
		// 一条内核规则都没有，就别去 exec nft 了。
		// 探针默认开着转发功能，绝大多数机器上一条规则都不会配，
		// 每 30 秒白跑一次外部命令既费资源又会在日志里刷噪音。
		// 表里就算有上一版留下的计数，也归不到任何一跳上，读了也是丢掉。
		return nil, nil
	}

	raw, err := readNftCounters()
	if err != nil {
		return nil, err
	}

	k.mu.Lock()
	// key 是具体协议 + 端口。tcp+udp 规则会占两个 key，都指向同一个 HopID。
	index := map[string]int64{}
	for _, r := range k.applied {
		for _, p := range splitProtos(r.Proto) {
			index[protoPortKey(p, r.ListenPort)] = r.HopID
		}
	}
	k.mu.Unlock()

	agg := map[int64]*Counter{}
	for _, c := range raw {
		hopID, ok := index[protoPortKey(c.Proto, c.ListenPort)]
		if !ok {
			// 表里还留着上一版规则的 counter（apply 和读取之间规则变了）。
			// 归不到任何一跳上，只能丢弃。
			continue
		}
		e := agg[hopID]
		if e == nil {
			e = &Counter{HopID: hopID}
			agg[hopID] = e
		}
		switch c.Direction {
		case "reply":
			e.BytesDown += c.Bytes
		default:
			e.BytesUp += c.Bytes
		}
	}

	out := make([]Counter, 0, len(agg))
	for _, e := range agg {
		out = append(out, *e)
	}
	return out, nil
}

// Close 清掉 nftables 表和 tc 树。
func (k *kernelBackend) Close() {
	if err := Flush(); err != nil {
		k.log.Warn("清理 nftables 规则失败", "错误", err)
	}
	tc.Clear(k.iface)
}

func protoPortKey(proto string, port int) string {
	return proto + "/" + strconv.Itoa(port)
}
