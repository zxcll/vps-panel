package server

import (
	"testing"

	"github.com/zxcll/vps-panel/internal/store"
)

// 保活选谁，是用户报的第二个 bug 的核心。
//
// 保活的职责是「被阿里云回收了就拉起来」，不是「只要停着就拉起来」。
// 少了 PlannedStop 这个判据，定时关机刚把机器停掉，保活下一轮就给拉回来了，
// 用户看到的现象就是「定时关机根本不生效」。
func TestKeepAliveTargets(t *testing.T) {
	insts := []*store.CDTInstance{
		{InstanceID: "被回收的抢占式", Guarded: true, IsSpot: true, PlannedStop: false},
		{InstanceID: "定时关机停掉的", Guarded: true, IsSpot: true, PlannedStop: true},
		{InstanceID: "非抢占式", Guarded: true, IsSpot: false, PlannedStop: false},
		{InstanceID: "没打守护标记", Guarded: false, IsSpot: true, PlannedStop: false},
		{InstanceID: "熔断停掉的", Guarded: true, IsSpot: true, PlannedStop: true},
	}

	got := keepAliveTargets(insts)
	if len(got) != 1 {
		var names []string
		for _, g := range got {
			names = append(names, g.InstanceID)
		}
		t.Fatalf("只有「被回收的抢占式」该被保活管，实际选中 %d 个：%v", len(got), names)
	}
	if got[0].InstanceID != "被回收的抢占式" {
		t.Errorf("选错了：%s", got[0].InstanceID)
	}
}

// 面板主动停的机器一律不碰 —— 定时关机、流量熔断、手动停机都算。
func TestKeepAliveNeverFightsPlannedStop(t *testing.T) {
	stopped := []*store.CDTInstance{
		{InstanceID: "i-1", Guarded: true, IsSpot: true, PlannedStop: true},
		{InstanceID: "i-2", Guarded: true, IsSpot: true, PlannedStop: true},
	}
	if got := keepAliveTargets(stopped); len(got) != 0 {
		t.Errorf("面板主动停的机器保活一个都不该碰，实际选中 %d 个 —— "+
			"这会让定时关机和保活互相拆台", len(got))
	}
}

func TestKeepAliveTargetsEmpty(t *testing.T) {
	if got := keepAliveTargets(nil); len(got) != 0 {
		t.Errorf("空输入应返回空，实际 %d", len(got))
	}
}
