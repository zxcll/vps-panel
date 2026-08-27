package server

import (
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

// 定时开关机的窗口判断。
//
// 单独拎出来做成纯函数，是因为「现在是不是在该关机的时段里」这件事有个
// 一眼看不出来的坑：窗口经常**跨午夜**。23:00 关、08:00 开，这个区间是
// [23:00, 24:00) ∪ [00:00, 08:00)，用一句 stop <= now && now < start 判会
// 得到完全相反的结果 —— 凌晨三点被判成「不在关机窗口」，于是保活把刚定时
// 关掉的机器又拉起来。
//
// 这是用户报的第二个 bug 的兜底：主路径靠实例上的 planned_stop 标记，
// 但那个标记可能因为用户在阿里云控制台手动动过、或者实例被重建而丢失。
// 有了窗口判断，即使标记没了，保活也不会在该关机的时段里把机器拉起来。

// inScheduledOffWindow 判断此刻是不是落在「定时关机 → 定时开机」这段里。
//
// 两个时间任意一个没配就返回 false —— 只配了关机没配开机的话，
// 「关机之后」是个没有尽头的区间，把它当成 off 窗口会让保活永远不生效，
// 那是另一种意外。那种配置下靠 planned_stop 标记本身就够了。
func inScheduledOffWindow(a *store.CDTAccount, now time.Time) bool {
	if a.AutoStopTime == "" || a.AutoStartTime == "" {
		return false
	}

	loc, err := time.LoadLocation(a.ScheduleTZ)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)

	stop, ok1 := parseClockMinutes(a.AutoStopTime)
	start, ok2 := parseClockMinutes(a.AutoStartTime)
	if !ok1 || !ok2 || stop == start {
		return false
	}
	cur := local.Hour()*60 + local.Minute()

	if stop < start {
		// 同一天内的窗口，比如 01:00 关、08:00 开。
		return cur >= stop && cur < start
	}
	// 跨午夜，比如 23:00 关、08:00 开：
	// 窗口是「当天 23:00 之后」**或**「次日 08:00 之前」。
	return cur >= stop || cur < start
}

// parseClockMinutes 把 "HH:MM" 转成「当天第几分钟」。
func parseClockMinutes(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}
