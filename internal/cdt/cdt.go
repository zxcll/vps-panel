// Package cdt 是阿里云 CDT 免费额度的判定逻辑：地域怎么分池、用了多少、
// 要不要熔断停机。
//
// 单独成包、全是纯函数，理由和 forwardplan 一样 —— 这里的判定下游接的是
// **真关机**。算错不是数字难看，是机器被误停。所以它必须能脱离网络和数据库
// 单独测。
//
// 关于 CDT 额度，有两件事最容易搞错，这个包的存在就是为了把它们钉住：
//
//  1. **只算出方向。** 入方向不计费，也不消耗免费额度。
//  2. **中国内地和非中国内地是两个独立的池**，不是一个总额度。
//     中国内地 20 GB/月，非中国内地 200 GB/月。把两边加起来对着 220 GB 判，
//     会在中国内地那 20 GB 早就跑满时仍然显示「才用了 10%」。
//
// 额度每自然月 1 日 0 点（北京时间）刷新，当月不结转。
package cdt

import (
	"fmt"
	"strings"
	"time"
)

// Bucket 是一个免费额度池。
type Bucket string

const (
	// BucketMainland 是中国内地。
	BucketMainland Bucket = "mainland"
	// BucketOverseas 是非中国内地（含香港、澳门、台湾）。
	BucketOverseas Bucket = "overseas"
)

// 默认免费额度（GB/月），和阿里云当前的规则一致。
// 做成可配置的，是因为额度规则会变，而且有的账号买了额外的包。
const (
	DefaultQuotaMainlandGB = 20
	DefaultQuotaOverseasGB = 200
)

// GB 是 1 GiB 的字节数。阿里云的流量按十进制 GB 标，但差异对判定影响很小，
// 而面板别处（fmtBytes、配额）一律用 1024 进制，这里跟着统一，
// 免得同一个页面上两种进制混着出现。
const GB int64 = 1 << 30

// Label 返回池子的中文名。
func (b Bucket) Label() string {
	if b == BucketMainland {
		return "中国内地"
	}
	return "非中国内地"
}

// mainlandPrefixes 之外的一切都算非中国内地。
//
// 判定规则：地域 ID 以 cn- 开头的是中国内地，但港澳台三个例外 ——
// 它们的地域 ID 也叫 cn-hongkong / cn-macau / cn-taipei，计费上却属于
// 非中国内地。漏掉这条例外，香港机器的流量会被算进只有 20 GB 的那个池，
// 于是一台正常跑的香港机器很快就会被误判成超额停机。
var overseasExceptions = map[string]bool{
	"cn-hongkong": true,
	"cn-macau":    true,
	"cn-taipei":   true,
}

// ClassifyRegion 判断一个业务地域属于哪个免费额度池。
func ClassifyRegion(businessRegionID string) Bucket {
	id := strings.ToLower(strings.TrimSpace(businessRegionID))
	if overseasExceptions[id] {
		return BucketOverseas
	}
	if strings.HasPrefix(id, "cn-") {
		return BucketMainland
	}
	return BucketOverseas
}

// Quota 是一个账号的两个额度池上限，单位字节。
type Quota struct {
	MainlandBytes int64
	OverseasBytes int64
}

// QuotaFromGB 从 GB 数构造 Quota。传 0 或负数即用默认额度 ——
// 「没配过」应该直接能用，而不是变成 0 额度当场熔断。
func QuotaFromGB(mainlandGB, overseasGB float64) Quota {
	return Quota{
		MainlandBytes: quotaBytes(mainlandGB, DefaultQuotaMainlandGB),
		OverseasBytes: quotaBytes(overseasGB, DefaultQuotaOverseasGB),
	}
}

func quotaBytes(gb float64, def float64) int64 {
	if gb <= 0 {
		gb = def
	}
	return int64(gb * float64(GB))
}

// Of 取某个池的额度上限。
func (q Quota) Of(b Bucket) int64 {
	if b == BucketMainland {
		return q.MainlandBytes
	}
	return q.OverseasBytes
}

// BucketUsage 是单个池的用量情况。
type BucketUsage struct {
	Bucket  Bucket  `json:"bucket"`
	Label   string  `json:"label"`
	Used    int64   `json:"used_bytes"`
	Quota   int64   `json:"quota_bytes"`
	Percent float64 `json:"percent"`
	// Exceeded 表示这个池已经越过熔断线。
	Exceeded bool `json:"exceeded"`
}

// Status 是一次完整的额度判定结果。
type Status struct {
	Buckets []BucketUsage `json:"buckets"`
	// Trip 为真表示至少有一个池越线，该熔断了。
	Trip bool `json:"trip"`
	// Reason 是给用户看的一句话，说明是哪个池、用了多少、线在哪。
	// Trip 为假时是空串。
	Reason string `json:"reason"`
}

// SumByBucket 把逐地域的流量明细汇总成两个池。
//
// traffic 的 key 是业务地域 ID，value 是该地域的出方向字节数。
func SumByBucket(traffic map[string]int64) map[Bucket]int64 {
	out := map[Bucket]int64{BucketMainland: 0, BucketOverseas: 0}
	for region, bytes := range traffic {
		out[ClassifyRegion(region)] += bytes
	}
	return out
}

// Evaluate 判断这个账号有没有越过熔断线。
//
// thresholdPercent 是熔断线，比如 95 表示用到额度的 95% 就停机。
// 传 0 或负数按 100 处理（跑满才停）；大于 100 的值原样接受 ——
// 有人会故意设成 110%，让免费额度用完后再跑一点点付费流量。
//
// **两个池分别判**，任意一个越线就熔断。不能把两边加起来对总额度判：
// 中国内地只有 20 GB，被 200 GB 的非中国内地一平均，跑爆了也看不出来。
func Evaluate(used map[Bucket]int64, quota Quota, thresholdPercent float64) Status {
	if thresholdPercent <= 0 {
		thresholdPercent = 100
	}

	st := Status{Buckets: make([]BucketUsage, 0, 2)}
	// 顺序固定，界面上不能每次刷新都换位置。
	for _, b := range []Bucket{BucketMainland, BucketOverseas} {
		limit := quota.Of(b)
		u := BucketUsage{
			Bucket: b,
			Label:  b.Label(),
			Used:   used[b],
			Quota:  limit,
		}
		if limit > 0 {
			u.Percent = float64(u.Used) / float64(limit) * 100
			u.Exceeded = u.Percent >= thresholdPercent
		}
		if u.Exceeded && !st.Trip {
			st.Trip = true
			st.Reason = fmt.Sprintf("%s流量已用 %s / %s（%.1f%%），达到 %.0f%% 熔断线",
				b.Label(), HumanBytes(u.Used), HumanBytes(limit), u.Percent, thresholdPercent)
		}
		st.Buckets = append(st.Buckets, u)
	}
	return st
}

// Cycle 是一个 CDT 账期，形如 "2026-08"。
type Cycle string

// beijing 是额度刷新用的时区。CDT 额度每自然月 1 日 0 点刷新，
// 按的是北京时间，不是面板所在机器的本地时间 —— 面板部署在美西的话，
// 用本地时间算会让账期整整错开一天。
var beijing = time.FixedZone("CST", 8*3600)

// CycleOf 返回某个时刻所属的账期。
func CycleOf(t time.Time) Cycle {
	return Cycle(t.In(beijing).Format("2006-01"))
}

// CurrentCycle 是此刻的账期。
func CurrentCycle() Cycle { return CycleOf(time.Now()) }

// Start 返回这个账期的起始时刻（北京时间月初零点）。
func (c Cycle) Start() (time.Time, error) {
	t, err := time.ParseInLocation("2006-01", string(c), beijing)
	if err != nil {
		return time.Time{}, fmt.Errorf("账期 %q 格式不对，应形如 2026-08", string(c))
	}
	return t, nil
}

// HumanBytes 把字节数格式化成人看的形式，和前端的 fmtBytes 保持一致的 1024 进制。
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
