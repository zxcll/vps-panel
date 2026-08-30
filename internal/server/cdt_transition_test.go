package server

import (
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/store"
)

func TestCDTSyncIgnoresLaggingRunningImmediatelyAfterStop(t *testing.T) {
	now := time.Now().UTC()
	old := &store.CDTInstance{
		Status: alicloud.StatusStopping, PlannedStop: true, UpdatedAt: now.Add(-15 * time.Second),
	}
	skip, clear := cdtInstanceSyncDecision(old, alicloud.StatusRunning, now)
	if !skip || clear {
		t.Fatalf("关机后滞后的 Running 应跳过且保留 planned_stop，实际 skip=%v clear=%v", skip, clear)
	}
}

func TestCDTSyncNeverTreatsStoppingToRunningAsRestart(t *testing.T) {
	now := time.Now().UTC()
	old := &store.CDTInstance{
		Status: alicloud.StatusStopping, PlannedStop: true,
		UpdatedAt: now.Add(-cdtStopStatusLagWindow - time.Second),
	}
	skip, clear := cdtInstanceSyncDecision(old, alicloud.StatusRunning, now)
	if skip {
		t.Fatal("超过滞后窗口后可以更新云端展示状态，不应永远卡在 Stopping")
	}
	if clear {
		t.Fatal("Stopping → Running 不能解除 planned_stop，否则实例真正停下后会被保活拉起")
	}
}

func TestCDTSyncClearsPlannedStopOnlyAfterRealStartTransition(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		oldStatus string
		remote    string
		planned   bool
		wantClear bool
	}{
		{name: "已停机后直接运行", oldStatus: alicloud.StatusStopped, remote: alicloud.StatusRunning, planned: true, wantClear: true},
		{name: "启动中变为运行", oldStatus: alicloud.StatusStarting, remote: alicloud.StatusRunning, planned: true, wantClear: true},
		{name: "仍在停机", oldStatus: alicloud.StatusStopped, remote: alicloud.StatusStopped, planned: true},
		{name: "普通实例不参与", oldStatus: alicloud.StatusStopped, remote: alicloud.StatusRunning, planned: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := &store.CDTInstance{Status: tc.oldStatus, PlannedStop: tc.planned, UpdatedAt: now.Add(-time.Hour)}
			_, got := cdtInstanceSyncDecision(old, tc.remote, now)
			if got != tc.wantClear {
				t.Errorf("clear=%v，期望 %v", got, tc.wantClear)
			}
		})
	}
}
