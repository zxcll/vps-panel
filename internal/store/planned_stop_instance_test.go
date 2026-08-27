package store

import (
	"context"
	"path/filepath"
	"testing"
)

// cdt_instances.planned_stop 是「定时关机和保活打架」这个 bug 的修法核心。
//
// 保活的职责是「被阿里云回收了就拉起来」，不是「只要停着就拉起来」。
// 少了这个标记，定时关机刚把机器停掉，保活下一轮就给拉回来了 ——
// 用户看到的现象是「定时关机根本不生效」。
func TestInstancePlannedStopRoundTrips(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	if err := st.UpsertCDTInstance(ctx, &CDTInstance{
		AccountID: a.ID, InstanceID: "i-1", Status: "Running", IsSpot: true,
	}); err != nil {
		t.Fatalf("写实例失败: %v", err)
	}
	list, _ := st.CDTInstancesOf(ctx, a.ID)
	if list[0].PlannedStop {
		t.Error("新同步进来的实例不该带计划内停机标记")
	}

	if err := st.SetCDTInstancePlannedStop(ctx, list[0].ID, true); err != nil {
		t.Fatalf("置位失败: %v", err)
	}
	list, _ = st.CDTInstancesOf(ctx, a.ID)
	if !list[0].PlannedStop {
		t.Fatal("置位没生效")
	}

	if err := st.SetCDTInstancePlannedStop(ctx, list[0].ID, false); err != nil {
		t.Fatalf("清位失败: %v", err)
	}
	list, _ = st.CDTInstancesOf(ctx, a.ID)
	if list[0].PlannedStop {
		t.Error("清位没生效")
	}
}

// 同步实例不能把这个标记冲掉 —— 和 guarded 一样，它是面板的状态，
// 不是从阿里云拉回来的字段。冲掉的话保活又会把定时关掉的机器拉起来。
func TestSyncKeepsInstancePlannedStop(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	inst := &CDTInstance{AccountID: a.ID, InstanceID: "i-1", Status: "Running", IsSpot: true}
	st.UpsertCDTInstance(ctx, inst)
	list, _ := st.CDTInstancesOf(ctx, a.ID)
	st.SetCDTInstancePlannedStop(ctx, list[0].ID, true)
	st.SetCDTInstanceGuarded(ctx, list[0].ID, true)

	// 下一轮同步：状态变成 Stopped。
	inst.Status = "Stopped"
	st.UpsertCDTInstance(ctx, inst)

	list, _ = st.CDTInstancesOf(ctx, a.ID)
	if list[0].Status != "Stopped" {
		t.Errorf("同步应更新状态，实际 %q", list[0].Status)
	}
	if !list[0].PlannedStop {
		t.Error("同步把 planned_stop 冲掉了 —— 保活会把定时关掉的机器又拉起来")
	}
	if !list[0].Guarded {
		t.Error("同步把 guarded 冲掉了")
	}
}

// 新列在老库上要能自动补齐且幂等（前几个版本踩过这个坑）。
func TestInstancePlannedStopColumnIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cdt.db")
	ctx := context.Background()

	st := openTestStore(t, path)
	a := newCDTTestAccount(t, st, "账号")
	st.UpsertCDTInstance(ctx, &CDTInstance{AccountID: a.ID, InstanceID: "i-1", Status: "Stopped"})
	list, _ := st.CDTInstancesOf(ctx, a.ID)
	st.SetCDTInstancePlannedStop(ctx, list[0].ID, true)
	st.Close()

	for i := range 3 {
		again, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次重放迁移失败: %v", i+1, err)
		}
		got, _ := again.CDTInstancesOf(ctx, a.ID)
		if len(got) != 1 || !got[0].PlannedStop {
			again.Close()
			t.Fatalf("第 %d 次重放后 planned_stop 丢了", i+1)
		}
		again.Close()
	}
}
