package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

// 定时关机窗口的判断有个一眼看不出来的坑：窗口经常**跨午夜**。
// 23:00 关、08:00 开，区间是 [23:00,24:00) ∪ [00:00,08:00)。
// 用一句 stop <= now && now < start 判会得到完全相反的结果 ——
// 凌晨三点被判成「不在关机窗口」，于是保活把刚定时关掉的机器又拉起来。
// 这正是用户报的第二个 bug。

func acct(stop, start string) *store.CDTAccount {
	return &store.CDTAccount{
		AutoStopTime: stop, AutoStartTime: start, ScheduleTZ: "Asia/Shanghai",
	}
}

func at(hour, min int) time.Time {
	return time.Date(2026, 8, 26, hour, min, 0, 0, time.FixedZone("CST", 8*3600))
}

// 跨午夜：23:00 关、08:00 开。
func TestOffWindowAcrossMidnight(t *testing.T) {
	a := acct("23:00", "08:00")

	inWindow := []struct{ h, m int }{
		{23, 0},  // 刚到关机点
		{23, 30}, // 当晚
		{0, 0},   // 午夜
		{3, 0},   // 凌晨——最容易判错的点
		{7, 59},  // 开机前一分钟
	}
	for _, tc := range inWindow {
		if !inScheduledOffWindow(a, at(tc.h, tc.m)) {
			t.Errorf("%02d:%02d 应在关机窗口内（23:00→08:00 跨午夜），"+
				"判错的话保活会把定时关掉的机器拉起来", tc.h, tc.m)
		}
	}

	outWindow := []struct{ h, m int }{
		{8, 0},  // 刚到开机点，窗口结束
		{12, 0}, // 白天
		{22, 59},
	}
	for _, tc := range outWindow {
		if inScheduledOffWindow(a, at(tc.h, tc.m)) {
			t.Errorf("%02d:%02d 不该在关机窗口内", tc.h, tc.m)
		}
	}
}

// 同一天内的窗口：01:00 关、08:00 开。
func TestOffWindowSameDay(t *testing.T) {
	a := acct("01:00", "08:00")

	if !inScheduledOffWindow(a, at(3, 0)) {
		t.Error("03:00 应在 01:00→08:00 的窗口内")
	}
	if inScheduledOffWindow(a, at(0, 30)) {
		t.Error("00:30 在关机点之前，不该算在窗口内")
	}
	if inScheduledOffWindow(a, at(9, 0)) {
		t.Error("09:00 在开机点之后，不该算在窗口内")
	}
}

// 只配了一头就不算窗口。
//
// 「只配关机不配开机」的话，「关机之后」是个没有尽头的区间，
// 把它当成 off 窗口会让保活永远不生效 —— 那是另一种意外。
// 这种配置下靠实例上的 planned_stop 标记本身就够了。
func TestOffWindowNeedsBothTimes(t *testing.T) {
	if inScheduledOffWindow(acct("23:00", ""), at(3, 0)) {
		t.Error("只配了关机时间不该算窗口")
	}
	if inScheduledOffWindow(acct("", "08:00"), at(3, 0)) {
		t.Error("只配了开机时间不该算窗口")
	}
	if inScheduledOffWindow(acct("", ""), at(3, 0)) {
		t.Error("两个都没配不该算窗口")
	}
}

// 两个时间相同等于没配，别把它当成 24 小时全天关机。
func TestOffWindowIgnoresEqualTimes(t *testing.T) {
	if inScheduledOffWindow(acct("08:00", "08:00"), at(12, 0)) {
		t.Error("关机和开机时间相同应当视为没配，而不是全天关机")
	}
}

// 时区要按账号配的算，不是按面板机器的本地时间。
// 面板部署在美西的话，用本地时间会整整错开半天。
func TestOffWindowUsesAccountTimezone(t *testing.T) {
	a := acct("23:00", "08:00")

	// UTC 19:00 = 北京时间次日 03:00，在窗口内。
	utc := time.Date(2026, 8, 26, 19, 0, 0, 0, time.UTC)
	if !inScheduledOffWindow(a, utc) {
		t.Error("应按账号配的时区（Asia/Shanghai）判断，不是面板本地时间")
	}

	// UTC 03:00 = 北京时间 11:00，不在窗口内。
	utc2 := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	if inScheduledOffWindow(a, utc2) {
		t.Error("北京时间 11:00 不该算在关机窗口内")
	}
}

// 时区名写错了不能让整个判断崩掉或乱来，回落 UTC 即可。
func TestOffWindowSurvivesBadTimezone(t *testing.T) {
	a := acct("23:00", "08:00")
	a.ScheduleTZ = "不是时区"
	// 只要不 panic、能给出一个确定的结果就行。
	_ = inScheduledOffWindow(a, at(3, 0))
}

// cdtStartGuarded 的窗口守卫依赖一个边界性质：**开机时刻本身不算在窗口内**。
// 定时开机就是在 now == AutoStartTime 那一刻调 cdtStartGuarded 的，
// 要是边界判成「还在窗口内」，定时开机会被自己的守卫挡掉 —— 机器再也不开了。
// 这条耦合不写下来，下次有人把 cur < start 改成 cur <= start 时不会有任何声音。
func TestOffWindowExcludesStartMomentSoScheduledPowerOnWorks(t *testing.T) {
	for _, a := range []*store.CDTAccount{acct("23:00", "08:00"), acct("01:00", "08:00")} {
		if inScheduledOffWindow(a, at(8, 0)) {
			t.Errorf("%s→%s：开机时刻不该算在关机窗口内，"+
				"否则定时开机会被自己的守卫挡住，机器永远开不起来",
				a.AutoStopTime, a.AutoStartTime)
		}
	}
}

// 关机窗口里不许自动拉起。保活、账期翻页解除熔断都会走 cdtStartGuarded，
// 堵在这一处就都管住了。
func TestStartGuardedSkipsDuringOffWindow(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// 时间用的是真实 now，所以构造一个「此刻必定落在窗口内」的账号：
	// 从一分钟前开始关、到 23 小时后才开。
	now := time.Now()
	a := &store.CDTAccount{
		Name:          "测试账号",
		AutoStopTime:  now.Add(-time.Minute).Format("15:04"),
		AutoStartTime: now.Add(-2 * time.Minute).Format("15:04"),
		ScheduleTZ:    now.Location().String(),
	}
	if !inScheduledOffWindow(a, now) {
		t.Fatalf("测试自身构造有问题：此刻应落在关机窗口内")
	}

	// 落在窗口内就该在碰阿里云之前直接返回。s 没有 store 也没有凭据，
	// 真往下走会 panic —— 这本身就是「确实没往下走」的证据。
	if got := s.cdtStartGuarded(context.Background(), a); got != nil {
		t.Errorf("关机窗口内不该拉起任何实例，实际拉起了 %v", got)
	}
}
