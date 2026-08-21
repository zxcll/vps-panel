package billing

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", name, err)
	}
	return loc
}

func TestCurrentCycle(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	ny := mustLoc(t, "America/New_York")

	cases := []struct {
		name      string
		now       time.Time
		resetDay  int
		loc       *time.Location
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			name:      "月中，周期在本月内",
			now:       time.Date(2026, 8, 20, 12, 0, 0, 0, sh),
			resetDay:  1,
			loc:       sh,
			wantStart: time.Date(2026, 8, 1, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 9, 1, 0, 0, 0, 0, sh),
		},
		{
			name:      "重置日还没到，周期起点在上月",
			now:       time.Date(2026, 8, 5, 12, 0, 0, 0, sh),
			resetDay:  15,
			loc:       sh,
			wantStart: time.Date(2026, 7, 15, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 8, 15, 0, 0, 0, 0, sh),
		},
		{
			name:      "恰好落在重置时刻，算作新周期的第一秒",
			now:       time.Date(2026, 8, 15, 0, 0, 0, 0, sh),
			resetDay:  15,
			loc:       sh,
			wantStart: time.Date(2026, 8, 15, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 9, 15, 0, 0, 0, 0, sh),
		},
		{
			name:      "31 号遇平年 2 月，落到 2/28",
			now:       time.Date(2026, 2, 10, 0, 0, 0, 0, sh),
			resetDay:  31,
			loc:       sh,
			wantStart: time.Date(2026, 1, 31, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 2, 28, 0, 0, 0, 0, sh),
		},
		{
			name:      "31 号遇闰年 2 月，落到 2/29",
			now:       time.Date(2028, 2, 10, 0, 0, 0, 0, sh),
			resetDay:  31,
			loc:       sh,
			wantStart: time.Date(2028, 1, 31, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2028, 2, 29, 0, 0, 0, 0, sh),
		},
		{
			name:      "31 号，2 月末之后回到 3/31",
			now:       time.Date(2026, 3, 1, 0, 0, 0, 0, sh),
			resetDay:  31,
			loc:       sh,
			wantStart: time.Date(2026, 2, 28, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 3, 31, 0, 0, 0, 0, sh),
		},
		{
			name:      "跨年：12 月 → 次年 1 月",
			now:       time.Date(2026, 12, 20, 0, 0, 0, 0, sh),
			resetDay:  5,
			loc:       sh,
			wantStart: time.Date(2026, 12, 5, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2027, 1, 5, 0, 0, 0, 0, sh),
		},
		{
			name:      "跨年往回：1 月初 → 上年 12 月",
			now:       time.Date(2026, 1, 2, 0, 0, 0, 0, sh),
			resetDay:  5,
			loc:       sh,
			wantStart: time.Date(2025, 12, 5, 0, 0, 0, 0, sh),
			wantEnd:   time.Date(2026, 1, 5, 0, 0, 0, 0, sh),
		},
		{
			name:      "美东时区，夏令时期间",
			now:       time.Date(2026, 7, 10, 0, 0, 0, 0, ny),
			resetDay:  1,
			loc:       ny,
			wantStart: time.Date(2026, 7, 1, 0, 0, 0, 0, ny),
			wantEnd:   time.Date(2026, 8, 1, 0, 0, 0, 0, ny),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := CurrentCycle(tc.now, tc.resetDay, tc.loc)
			if !start.Equal(tc.wantStart) {
				t.Errorf("周期起点 = %s，期望 %s", start, tc.wantStart.UTC())
			}
			if !end.Equal(tc.wantEnd) {
				t.Errorf("周期终点 = %s，期望 %s", end, tc.wantEnd.UTC())
			}
			if !start.Before(end) {
				t.Errorf("起点 %s 应早于终点 %s", start, end)
			}
			if start.Location() != time.UTC || end.Location() != time.UTC {
				t.Errorf("返回值应为 UTC，实际 %s / %s", start.Location(), end.Location())
			}
		})
	}
}

// 时区决定边界，同一个 UTC 时刻在不同时区可能属于不同周期。
func TestCurrentCycleTimezoneMatters(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")

	// UTC 时间 2026-07-31 20:00 = 上海时间 2026-08-01 04:00，已过 8/1 边界
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)

	shStart, _ := CurrentCycle(now, 1, sh)
	utcStart, _ := CurrentCycle(now, 1, time.UTC)

	if !shStart.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, sh)) {
		t.Errorf("上海时区周期起点 = %s，期望 2026-08-01 00:00 CST", shStart)
	}
	if !utcStart.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC 周期起点 = %s，期望 2026-07-01 00:00 UTC", utcStart)
	}
}

func TestNextCycle(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	_, end := CurrentCycle(time.Date(2026, 8, 20, 0, 0, 0, 0, sh), 1, sh)

	// 站在旧周期的终点上，NextCycle 应给出下一个完整周期
	start2, end2 := NextCycle(end, 1, sh)
	if !start2.Equal(end) {
		t.Errorf("新周期起点 %s 应等于旧周期终点 %s", start2, end)
	}
	if !end2.After(start2) {
		t.Errorf("新周期终点 %s 应晚于起点 %s", end2, start2)
	}
}

// 连续滚 36 个月，验证周期首尾相接、不重不漏。
func TestCyclesAreContiguous(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	for _, day := range []int{1, 15, 28, 29, 30, 31} {
		start, end := CurrentCycle(time.Date(2026, 1, 10, 0, 0, 0, 0, sh), day, sh)
		for i := 0; i < 36; i++ {
			ns, ne := NextCycle(end, day, sh)
			if !ns.Equal(end) {
				t.Fatalf("reset_day=%d 第 %d 轮出现断层：上一周期终点 %s，新周期起点 %s", day, i, end, ns)
			}
			if !ne.After(ns) {
				t.Fatalf("reset_day=%d 第 %d 轮周期长度非正：%s → %s", day, i, ns, ne)
			}
			start, end = ns, ne
		}
		_ = start
	}
}

func TestEnsureCycle(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// 边界为零值（新建节点）→ 需要初始化
	_, _, changed := EnsureCycle(now, time.Time{}, time.Time{}, 1, "UTC")
	if !changed {
		t.Error("零值边界应被判定为需要更新")
	}

	// 已经正确 → 不变
	s, e := CurrentCycle(now, 1, time.UTC)
	_, _, changed = EnsureCycle(now, s, e, 1, "UTC")
	if changed {
		t.Error("边界已正确时不应判定为需要更新")
	}

	// 用户把 reset_day 从 1 改成 15 → 需要更新
	_, _, changed = EnsureCycle(now, s, e, 15, "UTC")
	if !changed {
		t.Error("reset_day 变更后应重算边界")
	}
}

func TestLoadLocationFallback(t *testing.T) {
	loc, err := LoadLocation("Mars/Olympus")
	if err == nil {
		t.Error("非法时区名应返回错误")
	}
	if loc != time.UTC {
		t.Errorf("非法时区应回落到 UTC，实际 %s", loc)
	}

	loc, err = LoadLocation("")
	if err != nil || loc != time.UTC {
		t.Errorf("空时区名应返回 UTC 且无错误，实际 %s / %v", loc, err)
	}
}

func TestProgress(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if got := Progress(start, start, end); got != 0 {
		t.Errorf("周期刚开始进度应为 0，实际 %v", got)
	}
	if got := Progress(end, start, end); got != 1 {
		t.Errorf("周期结束进度应为 1，实际 %v", got)
	}
	mid := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	if got := Progress(mid, start, end); got < 0.49 || got > 0.51 {
		t.Errorf("周期中点进度应约为 0.5，实际 %v", got)
	}
	// 边界退化（start == end）不应 panic 或返回 NaN
	if got := Progress(start, start, start); got != 0 {
		t.Errorf("零长度周期进度应为 0，实际 %v", got)
	}
}

func TestDaysInMonth(t *testing.T) {
	cases := map[string]struct {
		y    int
		m    time.Month
		want int
	}{
		"2026年2月": {2026, time.February, 28},
		"2028年2月": {2028, time.February, 29},
		"2100年2月": {2100, time.February, 28}, // 整百非闰年
		"2000年2月": {2000, time.February, 29}, // 400 年闰
		"4月":      {2026, time.April, 30},
		"12月":     {2026, time.December, 31},
	}
	for name, tc := range cases {
		if got := DaysInMonth(tc.y, tc.m); got != tc.want {
			t.Errorf("%s: DaysInMonth = %d，期望 %d", name, got, tc.want)
		}
	}
}
