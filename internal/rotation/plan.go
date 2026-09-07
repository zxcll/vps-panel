// Package rotation coordinates CDT account windows, DNS and delayed power-off.
package rotation

import (
	"fmt"
	"sort"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

const GracePeriod = 30 * time.Minute

func minute(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || t.Format("15:04") != s {
		return 0, fmt.Errorf("时间 %q 应为 HH:MM（午夜填 00:00）", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func Validate(c store.CDTRotation) error {
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("换班时区无效：%w", err)
	}
	if c.DNSRecordID <= 0 || len(c.Slots) == 0 {
		return fmt.Errorf("请选择 DDNS 记录并添加账号窗口")
	}
	used := [1440]bool{}
	accounts := map[int64]bool{}
	for _, s := range c.Slots {
		if s.AccountID <= 0 || s.InstanceID <= 0 {
			return fmt.Errorf("每个窗口都需要选择账号和实例")
		}
		if accounts[s.AccountID] {
			return fmt.Errorf("同一账号只能配置一个窗口")
		}
		accounts[s.AccountID] = true
		start, err := minute(s.Start)
		if err != nil {
			return err
		}
		end, err := minute(s.End)
		if err != nil {
			return err
		}
		if start == end && len(c.Slots) != 1 {
			return fmt.Errorf("多账号窗口的开始和结束时间不能相同")
		}
		duration := (end - start + 1440) % 1440
		if duration == 0 {
			duration = 1440
		}
		for n := 0; n < duration; n++ {
			m := (start + n) % 1440
			if used[m] {
				return fmt.Errorf("窗口在 %02d:%02d 重叠", m/60, m%60)
			}
			used[m] = true
		}
	}
	for m, ok := range used {
		if !ok {
			return fmt.Errorf("窗口需要覆盖全天，%02d:%02d 尚未分配", m/60, m%60)
		}
	}
	return nil
}

// Candidates starts with the scheduled account, then walks subsequent windows.
// Only quota/disabled exclusions cause fallback; a failed DNS write never does.
func Candidates(c store.CDTRotation, now time.Time) []store.CDTRotationSlot {
	slots := append([]store.CDTRotationSlot(nil), c.Slots...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].Start < slots[j].Start })
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil
	}
	t := now.In(loc)
	cur := t.Hour()*60 + t.Minute()
	for i, s := range slots {
		start, _ := minute(s.Start)
		end, _ := minute(s.End)
		inside := start == end || (start < end && cur >= start && cur < end) || (start > end && (cur >= start || cur < end))
		if inside {
			return append(slots[i:], slots[:i]...)
		}
	}
	return nil
}
