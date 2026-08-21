// Package billing 计算流量账单周期的边界。
//
// 每个节点自己设"每月几号清零"和时区，因为不同商家的账单日不一样，
// 而且商家用的是自己机房所在时区。边界一律按节点时区的当天 00:00:00 计算，
// 对外返回 UTC 时间。
//
// 边界情况：
//   - reset_day = 31 遇到 2 月/小月时，自动落到当月最后一天
//   - 时区含夏令时切换时，time.Date 会规整到当天实际存在的时刻
package billing

import (
	"fmt"
	"time"
)

// LoadLocation 解析时区名，失败时回落到 UTC 并返回错误。
// 调用方通常忽略错误直接用返回的 location —— 时区配错不应该让流量统计整个停摆。
func LoadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, fmt.Errorf("无法解析时区 %q，已回落到 UTC: %w", name, err)
	}
	return loc, nil
}

// DaysInMonth 返回某年某月的天数。
func DaysInMonth(year int, month time.Month) int {
	// 下个月的第 0 天 = 当月最后一天
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// addMonths 在 (year, month) 上加减若干个月。
func addMonths(year int, month time.Month, delta int) (int, time.Month) {
	total := year*12 + (int(month) - 1) + delta
	return total / 12, time.Month(total%12) + 1
}

// boundaryIn 返回指定年月的重置时刻。resetDay 超过当月天数时落到月末。
func boundaryIn(year int, month time.Month, resetDay int, loc *time.Location) time.Time {
	day := resetDay
	if day < 1 {
		day = 1
	}
	if d := DaysInMonth(year, month); day > d {
		day = d
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// CurrentCycle 返回 now 所属账单周期的 [start, end)，均为 UTC。
// start 是最近一次已发生的重置时刻，end 是下一次重置时刻。
func CurrentCycle(now time.Time, resetDay int, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)

	thisMonth := boundaryIn(local.Year(), local.Month(), resetDay, loc)

	if local.Before(thisMonth) {
		// 本月的重置日还没到 → 周期起点在上个月
		py, pm := addMonths(local.Year(), local.Month(), -1)
		start = boundaryIn(py, pm, resetDay, loc)
		end = thisMonth
	} else {
		start = thisMonth
		ny, nm := addMonths(local.Year(), local.Month(), 1)
		end = boundaryIn(ny, nm, resetDay, loc)
	}
	return start.UTC(), end.UTC()
}

// NextCycle 返回紧接在 after 之后的那个周期。
// 用于周期滚动：归档完旧周期后，算出新周期的边界。
func NextCycle(after time.Time, resetDay int, loc *time.Location) (start, end time.Time) {
	// 往后挪一秒，保证落进下一个周期而不是停在边界上
	return CurrentCycle(after.Add(time.Second), resetDay, loc)
}

// EnsureCycle 校正一个节点记录里的周期边界。
// 返回值 changed 表示边界需要更新（首次初始化，或用户改了 reset_day/时区）。
func EnsureCycle(now, curStart, curEnd time.Time, resetDay int, tz string) (start, end time.Time, changed bool) {
	loc, _ := LoadLocation(tz)
	want0, want1 := CurrentCycle(now, resetDay, loc)
	if curStart.Equal(want0) && curEnd.Equal(want1) {
		return curStart, curEnd, false
	}
	return want0, want1, true
}

// Progress 返回当前周期已过去的比例（0~1），用于前端展示"周期进度"。
func Progress(now, start, end time.Time) float64 {
	total := end.Sub(start)
	if total <= 0 {
		return 0
	}
	elapsed := now.Sub(start)
	switch {
	case elapsed <= 0:
		return 0
	case elapsed >= total:
		return 1
	default:
		return float64(elapsed) / float64(total)
	}
}
