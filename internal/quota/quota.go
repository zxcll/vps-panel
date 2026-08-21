// Package quota 负责把原始收发字节换算成"计费流量"，并判断是否触发预警/超额。
//
// 计费口径由节点的 billing_mode 决定：
//   - sum（双向）：rx + tx，最常见
//   - max（单向）：max(rx, tx)。用户说的"单向流量"就是这个——商家只按进出中
//     较大的那个方向计费，另一个方向白送
//   - out / in：只算出站或只算入站
//
// traffic_ratio 是校准系数。/proc/net/dev 统计的是网卡层字节，
// 和商家的计量口径通常差几个百分点，用它抹平。
package quota

import "github.com/zxcll/vps-panel/internal/store"

// Billed 把收发字节按指定口径换算成计费流量。
func Billed(rx, tx int64, mode string, ratio float64) int64 {
	if rx < 0 {
		rx = 0
	}
	if tx < 0 {
		tx = 0
	}

	var b int64
	switch mode {
	case store.BillingMax:
		b = rx
		if tx > b {
			b = tx
		}
	case store.BillingOut:
		b = tx
	case store.BillingIn:
		b = rx
	default: // store.BillingSum 及任何未知值
		b = rx + tx
	}

	if ratio > 0 && ratio != 1 {
		b = int64(float64(b) * ratio)
	}
	return b
}

// ForwardShare 换算一条转发规则**实际消耗掉多少机器流量**（界面上叫「实际消耗流量」）。
//
// 为什么需要这个换算：转发计数是按 conntrack 方向统计的，上行 up、下行 down；
// 而节点账本统计的是网卡字节。中转流量在网卡上要进出各走一遍 ——
// 客户端发来的 up 字节先入站再出站，目标回来的 down 字节也是先入站再出站，
// 于是网卡上 rx += up+down、tx += up+down。代入各种计费口径就得到：
//
//	sum（双向相加）：(up+down) + (up+down) = 2×(up+down)
//	max（单向取大）：max(up+down, up+down) = up+down
//	out / in：       up+down
//
// 有了它，面板才能把「这条规则转了 50G」和「这台机器用了 100G」对上号，
// 否则用户会以为账算错了。
//
// 注意这个函数只用于展示。转发流量**已经**通过网卡计数器计入配额了，
// 千万不要把结果再加进 node_usage —— 那才是真正的重复计费。
func ForwardShare(up, down int64, mode string, ratio float64) int64 {
	if up < 0 {
		up = 0
	}
	if down < 0 {
		down = 0
	}
	total := up + down
	if mode == store.BillingSum || !ValidMode(mode) {
		// 未知口径按 sum 处理，和 Billed 的兜底保持一致。
		total *= 2
	}
	if ratio > 0 && ratio != 1 {
		total = int64(float64(total) * ratio)
	}
	return total
}

// ValidMode 判断计费口径是否合法。
func ValidMode(mode string) bool {
	switch mode {
	case store.BillingSum, store.BillingMax, store.BillingOut, store.BillingIn:
		return true
	}
	return false
}

// Status 是一次配额评估的结果。
type Status struct {
	Billed    int64   `json:"billed_bytes"`
	Quota     int64   `json:"quota_bytes"`
	Remaining int64   `json:"remaining_bytes"` // 不限量时为 -1
	Percent   float64 `json:"percent"`         // 已用百分比；不限量时恒为 0
	Exceeded  bool    `json:"exceeded"`
	Warning   bool    `json:"warning"` // 达到预警线但还没超
}

// Evaluate 评估一个节点当前的配额状态。quota <= 0 表示不限量。
func Evaluate(rx, tx, quota int64, mode string, ratio float64, warnPercent int) Status {
	billed := Billed(rx, tx, mode, ratio)

	st := Status{Billed: billed, Quota: quota, Remaining: -1}
	if quota <= 0 {
		return st
	}

	st.Percent = float64(billed) / float64(quota) * 100
	st.Remaining = quota - billed
	if st.Remaining < 0 {
		st.Remaining = 0
	}
	st.Exceeded = billed >= quota

	if warnPercent > 0 && warnPercent < 100 && !st.Exceeded {
		st.Warning = st.Percent >= float64(warnPercent)
	}
	return st
}

// EvaluateNode 是 Evaluate 的便捷包装。
func EvaluateNode(n *store.Node, u *store.Usage) Status {
	var rx, tx int64
	if u != nil {
		rx, tx = u.RxBytes, u.TxBytes
	}
	return Evaluate(rx, tx, n.QuotaBytes, n.BillingMode, n.TrafficRatio, n.WarnPercent)
}
