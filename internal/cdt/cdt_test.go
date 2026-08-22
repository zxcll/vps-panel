package cdt

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyRegion(t *testing.T) {
	cases := []struct {
		region string
		want   Bucket
	}{
		{"cn-hangzhou", BucketMainland},
		{"cn-beijing", BucketMainland},
		{"cn-shanghai", BucketMainland},
		{"cn-shenzhen", BucketMainland},
		{"cn-zhangjiakou", BucketMainland},

		// 港澳台的地域 ID 也是 cn- 开头，计费上却算非中国内地。
		// 漏掉这条例外，一台正常跑的香港机器会被算进只有 20GB 的池子，
		// 很快就被误判成超额、然后被自动停机。
		{"cn-hongkong", BucketOverseas},
		{"cn-macau", BucketOverseas},
		{"cn-taipei", BucketOverseas},

		{"ap-southeast-1", BucketOverseas},
		{"ap-northeast-1", BucketOverseas},
		{"us-west-1", BucketOverseas},
		{"eu-central-1", BucketOverseas},

		// 大小写和空格不该影响判定。
		{"  CN-HONGKONG  ", BucketOverseas},
		{"CN-Hangzhou", BucketMainland},

		// 不认识的地域一律算非中国内地：那个池大得多，
		// 猜错的后果是「晚一点熔断」而不是「误停一台正常的机器」。
		{"", BucketOverseas},
		{"unknown-region", BucketOverseas},
	}

	for _, tc := range cases {
		if got := ClassifyRegion(tc.region); got != tc.want {
			t.Errorf("ClassifyRegion(%q) = %q，期望 %q", tc.region, got, tc.want)
		}
	}
}

func TestSumByBucket(t *testing.T) {
	got := SumByBucket(map[string]int64{
		"cn-hangzhou":    3 * GB,
		"cn-beijing":     2 * GB,
		"cn-hongkong":    50 * GB,
		"ap-southeast-1": 30 * GB,
	})
	if got[BucketMainland] != 5*GB {
		t.Errorf("中国内地应汇总成 5GB，实际 %d", got[BucketMainland])
	}
	// 香港必须落在非中国内地这一侧。
	if got[BucketOverseas] != 80*GB {
		t.Errorf("非中国内地应汇总成 80GB（含香港 50GB），实际 %d", got[BucketOverseas])
	}
}

// 空输入也要给出两个池的零值，界面上不能少一行。
func TestSumByBucketAlwaysHasBothBuckets(t *testing.T) {
	got := SumByBucket(nil)
	if len(got) != 2 {
		t.Fatalf("应始终返回两个池，实际 %d 个", len(got))
	}
}

func TestEvaluateTripsPerBucket(t *testing.T) {
	quota := QuotaFromGB(20, 200)

	// 非中国内地才用了 10%，但中国内地已经 100% —— 必须熔断。
	// 这正是「把两个池加起来对总额度判」会漏掉的情况：
	// (20+20)/(20+200) 才 18%，看着一点事都没有。
	st := Evaluate(map[Bucket]int64{
		BucketMainland: 20 * GB,
		BucketOverseas: 20 * GB,
	}, quota, 95)

	if !st.Trip {
		t.Fatal("中国内地池已跑满，应当熔断")
	}
	if !strings.Contains(st.Reason, "中国内地") {
		t.Errorf("熔断原因应指明是哪个池，实际 %q", st.Reason)
	}
}

func TestEvaluateNoTripWhenBothUnderThreshold(t *testing.T) {
	quota := QuotaFromGB(20, 200)
	st := Evaluate(map[Bucket]int64{
		BucketMainland: 10 * GB,
		BucketOverseas: 100 * GB,
	}, quota, 95)

	if st.Trip {
		t.Errorf("两个池都在 50%%，不该熔断：%s", st.Reason)
	}
	if st.Reason != "" {
		t.Errorf("没熔断时 Reason 应为空，实际 %q", st.Reason)
	}
	if len(st.Buckets) != 2 {
		t.Fatalf("应返回两个池的用量，实际 %d", len(st.Buckets))
	}
	// 顺序固定：界面上不能每次刷新都换位置。
	if st.Buckets[0].Bucket != BucketMainland || st.Buckets[1].Bucket != BucketOverseas {
		t.Error("池的顺序应固定为「中国内地、非中国内地」")
	}
	if st.Buckets[0].Percent != 50 {
		t.Errorf("中国内地应是 50%%，实际 %.2f", st.Buckets[0].Percent)
	}
}

// 熔断线的边界：正好等于阈值就该触发（和节点配额的口径保持一致）。
func TestEvaluateThresholdBoundary(t *testing.T) {
	quota := QuotaFromGB(100, 100)

	at := Evaluate(map[Bucket]int64{BucketMainland: 95 * GB}, quota, 95)
	if !at.Trip {
		t.Error("用量正好等于熔断线时应触发")
	}

	just := Evaluate(map[Bucket]int64{BucketMainland: 94 * GB}, quota, 95)
	if just.Trip {
		t.Error("还差一点时不该触发")
	}
}

func TestEvaluateThresholdDefaults(t *testing.T) {
	quota := QuotaFromGB(100, 100)

	// 阈值传 0 应按 100% 处理，而不是「0% 就熔断」——
	// 后者会让没配过阈值的账号一上来就被全停。
	st := Evaluate(map[Bucket]int64{BucketMainland: 1 * GB}, quota, 0)
	if st.Trip {
		t.Fatalf("阈值为 0 时应按 100%% 处理，不该熔断：%s", st.Reason)
	}

	full := Evaluate(map[Bucket]int64{BucketMainland: 100 * GB}, quota, 0)
	if !full.Trip {
		t.Error("阈值为 0 按 100%% 处理，跑满时应熔断")
	}

	// 有人会故意设成 110%，让免费额度用完后再跑一点付费流量。
	over := Evaluate(map[Bucket]int64{BucketMainland: 105 * GB}, quota, 110)
	if over.Trip {
		t.Error("阈值 110%% 时用到 105%% 不该熔断")
	}
}

func TestQuotaFromGBFallsBackToDefaults(t *testing.T) {
	// 没配过额度的账号应该直接能用，而不是变成 0 额度当场熔断。
	q := QuotaFromGB(0, 0)
	if q.MainlandBytes != DefaultQuotaMainlandGB*GB {
		t.Errorf("中国内地应回落默认 20GB，实际 %d", q.MainlandBytes)
	}
	if q.OverseasBytes != DefaultQuotaOverseasGB*GB {
		t.Errorf("非中国内地应回落默认 200GB，实际 %d", q.OverseasBytes)
	}

	st := Evaluate(map[Bucket]int64{BucketMainland: 1 * GB}, q, 95)
	if st.Trip {
		t.Errorf("回落默认额度后不该立刻熔断：%s", st.Reason)
	}
}

func TestCycleUsesBeijingTime(t *testing.T) {
	// UTC 的 7 月 31 日 17:00 已经是北京时间 8 月 1 日 01:00，属于 8 月账期。
	// 用面板所在机器的本地时间算，部署在美西时整整会错开一天。
	utc := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	if got := CycleOf(utc); got != "2026-08" {
		t.Errorf("CycleOf(%s) = %q，期望 2026-08", utc, got)
	}

	// 北京时间 7 月 31 日 23:59 仍属于 7 月。
	stillJuly := time.Date(2026, 7, 31, 15, 59, 0, 0, time.UTC)
	if got := CycleOf(stillJuly); got != "2026-07" {
		t.Errorf("CycleOf(%s) = %q，期望 2026-07", stillJuly, got)
	}
}

func TestCycleStart(t *testing.T) {
	start, err := Cycle("2026-08").Start()
	if err != nil {
		t.Fatalf("解析账期失败: %v", err)
	}
	// 北京时间 8 月 1 日零点 = UTC 7 月 31 日 16:00。
	want := time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC)
	if !start.UTC().Equal(want) {
		t.Errorf("账期起点 = %s，期望 %s", start.UTC(), want)
	}

	if _, err := Cycle("不是账期").Start(); err == nil {
		t.Error("非法账期应报错")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		512:       "512 B",
		1024:      "1.00 KiB",
		20 * GB:   "20.00 GiB",
		1536 * GB: "1.50 TiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// 「每天还能用多少」= (熔断线 - 已用) ÷ 本账期剩余天数。
//
// 比单看百分比有用：进度条说「用了 40%」，你还得自己算今天几号、还剩几天、
// 这个速度撑不撑得到月底。这个数直接回答那个问题。
func TestDailyBudget(t *testing.T) {
	quota := QuotaFromGB(100, 100)
	// 8 月 22 日（北京时间），31 天的月份 → 含今天还剩 10 天。
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

	// 熔断线 100%，已用 50GB，还剩 50GB / 10 天 = 5GB/天。
	st := EvaluateAt(map[Bucket]int64{BucketMainland: 50 * GB}, quota, 100, at)
	if st.DaysLeft != 10 {
		t.Fatalf("8 月 22 日应剩 10 天（含今天），实际 %d", st.DaysLeft)
	}
	if got := st.Buckets[0].DailyBudget; got != 5*GB {
		t.Errorf("每天预算应是 5GB，实际 %s", HumanBytes(got))
	}
}

// 预算要按**熔断线**算，不是按额度上限。
// 用户设了 95% 就是打算停在 95%，剩下那 5% 不该算进「还能用」里。
func TestDailyBudgetRespectsThreshold(t *testing.T) {
	quota := QuotaFromGB(100, 100)
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

	st := EvaluateAt(map[Bucket]int64{BucketMainland: 45 * GB}, quota, 95, at)
	// (95 - 45) / 10 = 5GB/天，而不是 (100-45)/10 = 5.5GB/天。
	if got := st.Buckets[0].DailyBudget; got != 5*GB {
		t.Errorf("应按 95%% 熔断线算出 5GB/天，实际 %s（按 100%% 算会是 5.5GB）",
			HumanBytes(got))
	}
}

// 已经越线了就没有预算可言，别给个负数或者绕回大正数。
func TestDailyBudgetZeroWhenExceeded(t *testing.T) {
	quota := QuotaFromGB(100, 100)
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

	st := EvaluateAt(map[Bucket]int64{BucketMainland: 120 * GB}, quota, 95, at)
	if got := st.Buckets[0].DailyBudget; got != 0 {
		t.Errorf("越线之后预算应是 0，实际 %d", got)
	}
	if !st.Buckets[0].Exceeded {
		t.Error("用超了却没标成越线")
	}
}

func TestDaysLeftInCycle(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 8, 1, 0, 0, 0, 0, cst), 31}, // 8 月 31 天
		{time.Date(2026, 8, 22, 12, 0, 0, 0, cst), 10},
		{time.Date(2026, 8, 31, 23, 59, 0, 0, cst), 1}, // 最后一天，含今天算 1
		{time.Date(2026, 2, 1, 0, 0, 0, 0, cst), 28},   // 平年 2 月
		{time.Date(2028, 2, 1, 0, 0, 0, 0, cst), 29},   // 闰年 2 月
	}
	for _, tc := range cases {
		if got := DaysLeftInCycle(tc.at); got != tc.want {
			t.Errorf("DaysLeftInCycle(%s) = %d，期望 %d",
				tc.at.Format("2006-01-02"), got, tc.want)
		}
	}

	// 账期按北京时间切：UTC 8 月 31 日 17:00 已经是北京 9 月 1 日，
	// 该算成 9 月的第一天（30 天）。
	utc := time.Date(2026, 8, 31, 17, 0, 0, 0, time.UTC)
	if got := DaysLeftInCycle(utc); got != 30 {
		t.Errorf("跨时区边界算错了：%d，期望 30（已经是北京时间 9 月 1 日）", got)
	}
}
