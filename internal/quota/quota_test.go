package quota

import (
	"testing"

	"github.com/zxcll/vps-panel/internal/store"
)

const gb = int64(1) << 30

func TestBilled(t *testing.T) {
	cases := []struct {
		name   string
		rx, tx int64
		mode   string
		ratio  float64
		want   int64
	}{
		{"双向：进出相加", 3 * gb, 7 * gb, store.BillingSum, 1, 10 * gb},
		{"单向：出站更大时取出站", 3 * gb, 7 * gb, store.BillingMax, 1, 7 * gb},
		{"单向：入站更大时取入站", 9 * gb, 7 * gb, store.BillingMax, 1, 9 * gb},
		{"单向：进出相等", 5 * gb, 5 * gb, store.BillingMax, 1, 5 * gb},
		{"仅出站", 9 * gb, 7 * gb, store.BillingOut, 1, 7 * gb},
		{"仅入站", 9 * gb, 7 * gb, store.BillingIn, 1, 9 * gb},
		{"未知口径按双向处理", 3 * gb, 7 * gb, "bogus", 1, 10 * gb},
		{"倍率 0.5 打对折", 4 * gb, 6 * gb, store.BillingSum, 0.5, 5 * gb},
		{"倍率 1.1 上浮", 10 * gb, 0, store.BillingSum, 1.1, int64(float64(10*gb) * 1.1)},
		{"倍率为 0 视为不校准", 4 * gb, 6 * gb, store.BillingSum, 0, 10 * gb},
		{"负数按 0 处理", -5, 7 * gb, store.BillingSum, 1, 7 * gb},
		{"全零", 0, 0, store.BillingMax, 1, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Billed(tc.rx, tc.tx, tc.mode, tc.ratio); got != tc.want {
				t.Errorf("Billed = %d，期望 %d", got, tc.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Run("未超未警", func(t *testing.T) {
		st := Evaluate(10*gb, 10*gb, 100*gb, store.BillingSum, 1, 90)
		if st.Exceeded || st.Warning {
			t.Errorf("20/100 GB 不该触发任何告警，实际 %+v", st)
		}
		if st.Remaining != 80*gb {
			t.Errorf("剩余量 = %d，期望 %d", st.Remaining, 80*gb)
		}
		if st.Percent < 19.9 || st.Percent > 20.1 {
			t.Errorf("百分比 = %v，期望约 20", st.Percent)
		}
	})

	t.Run("达到预警线", func(t *testing.T) {
		st := Evaluate(45*gb, 45*gb, 100*gb, store.BillingSum, 1, 90)
		if !st.Warning {
			t.Errorf("90/100 GB 应触发预警，实际 %+v", st)
		}
		if st.Exceeded {
			t.Error("90/100 GB 不该判定为超额")
		}
	})

	t.Run("恰好用满即算超额", func(t *testing.T) {
		st := Evaluate(50*gb, 50*gb, 100*gb, store.BillingSum, 1, 90)
		if !st.Exceeded {
			t.Errorf("用满配额应判定超额，实际 %+v", st)
		}
		if st.Warning {
			t.Error("已超额时不应再重复报预警")
		}
		if st.Remaining != 0 {
			t.Errorf("剩余量应为 0，实际 %d", st.Remaining)
		}
	})

	t.Run("超额后剩余量不为负", func(t *testing.T) {
		st := Evaluate(200*gb, 0, 100*gb, store.BillingSum, 1, 90)
		if st.Remaining != 0 {
			t.Errorf("剩余量 = %d，应被夹到 0", st.Remaining)
		}
	})

	t.Run("不限量", func(t *testing.T) {
		st := Evaluate(9999*gb, 9999*gb, 0, store.BillingSum, 1, 90)
		if st.Exceeded || st.Warning {
			t.Errorf("配额为 0 表示不限量，不该告警，实际 %+v", st)
		}
		if st.Remaining != -1 {
			t.Errorf("不限量时剩余量应为 -1，实际 %d", st.Remaining)
		}
	})

	t.Run("单向口径改变超额判定", func(t *testing.T) {
		// 双向 100GB 会超；单向取大只有 60GB，不超
		sum := Evaluate(40*gb, 60*gb, 100*gb, store.BillingSum, 1, 90)
		max := Evaluate(40*gb, 60*gb, 100*gb, store.BillingMax, 1, 90)
		if !sum.Exceeded {
			t.Error("双向口径下 40+60=100GB 应超额")
		}
		if max.Exceeded {
			t.Error("单向口径下 max(40,60)=60GB 不该超额")
		}
	})

	t.Run("预警百分比为 0 时不报预警", func(t *testing.T) {
		st := Evaluate(99*gb, 0, 100*gb, store.BillingSum, 1, 0)
		if st.Warning {
			t.Error("warn_percent=0 表示关闭预警")
		}
	})
}

func TestEvaluateNode(t *testing.T) {
	n := &store.Node{
		QuotaBytes:   100 * gb,
		BillingMode:  store.BillingMax,
		TrafficRatio: 1,
		WarnPercent:  80,
	}

	st := EvaluateNode(n, &store.Usage{RxBytes: 30 * gb, TxBytes: 85 * gb})
	if st.Exceeded {
		t.Errorf("max(30,85)=85GB 未达 100GB 配额，不该超额，实际 %+v", st)
	}
	if !st.Warning {
		t.Errorf("85%% 已过 80%% 预警线，应报预警，实际 %+v", st)
	}

	// usage 为 nil（节点从未上报过）不应 panic
	st = EvaluateNode(n, nil)
	if st.Billed != 0 {
		t.Errorf("无用量记录时计费流量应为 0，实际 %d", st.Billed)
	}
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{store.BillingSum, store.BillingMax, store.BillingOut, store.BillingIn} {
		if !ValidMode(m) {
			t.Errorf("%q 应是合法口径", m)
		}
	}
	for _, m := range []string{"", "SUM", "both", "bogus"} {
		if ValidMode(m) {
			t.Errorf("%q 不该被判为合法口径", m)
		}
	}
}

func TestForwardShare(t *testing.T) {
	// 一条规则跑了上行 3G、下行 7G。中转流量在网卡上进出各一遍，
	// 所以它在节点账本里的体现是 rx += 10G、tx += 10G。
	const up, down = 3 * gb, 7 * gb

	cases := []struct {
		name  string
		mode  string
		ratio float64
		want  int64
	}{
		// sum 口径下 rx 和 tx 相加，所以是 2 倍。
		{"双向：进出各算一遍", store.BillingSum, 1, 20 * gb},
		// 单向口径下进出相等，取大还是 10G。
		{"单向：进出相等取其一", store.BillingMax, 1, 10 * gb},
		{"仅出站", store.BillingOut, 1, 10 * gb},
		{"仅入站", store.BillingIn, 1, 10 * gb},
		{"未知口径按双向处理", "bogus", 1, 20 * gb},
		{"倍率跟着乘", store.BillingMax, 1.1, int64(float64(10*gb) * 1.1)},
		{"倍率为 0 视为不校准", store.BillingMax, 0, 10 * gb},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForwardShare(up, down, tc.mode, tc.ratio); got != tc.want {
				t.Errorf("ForwardShare = %d，期望 %d", got, tc.want)
			}
		})
	}

	t.Run("负数按 0 处理", func(t *testing.T) {
		if got := ForwardShare(-1, -1, store.BillingSum, 1); got != 0 {
			t.Errorf("ForwardShare = %d，期望 0", got)
		}
	})
}

// ForwardShare 只是展示用的换算，绝不能反过来影响配额判定。
// 这条用例钉住"quota 包里没有任何路径会把转发流量加进 Billed"。
func TestForwardShareDoesNotAffectBilled(t *testing.T) {
	before := Billed(10*gb, 10*gb, store.BillingSum, 1)
	_ = ForwardShare(5*gb, 5*gb, store.BillingSum, 1)
	after := Billed(10*gb, 10*gb, store.BillingSum, 1)
	if before != after {
		t.Errorf("Billed 的结果不该被 ForwardShare 影响：%d → %d", before, after)
	}
}
