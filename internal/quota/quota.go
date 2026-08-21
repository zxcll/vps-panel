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
