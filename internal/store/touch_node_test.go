package store

import (
	"context"
	"testing"
	"time"
)

// TouchNode 走的是**每一次探针上报**，比 engine 的 30 秒轮询频繁得多。
// 所以「哪些状态不能被心跳覆盖」必须在这里守住 —— 只在 engine 里守是不够的，
// 心跳会抢先把状态改掉。
//
// planned_stop 就在这里漏过一次：定时关机之后，关机窗口里的每一次心跳都把
// 节点刷成 online，等机器真断电就误报了一条掉线。用户报的
// 「关联了 CDT 定时关机还是提示离线」主因就在这。
func TestTouchNodeDoesNotOverridePanelOwnedStatus(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, status := range panelOwnedStatuses {
		n := newForwardTestNode(t, st, "节点-"+status)
		if err := st.SetNodeStatus(ctx, n.ID, status); err != nil {
			t.Fatalf("置状态失败: %v", err)
		}

		// 探针在机器关掉之前还会再报几次。
		if err := st.TouchNode(ctx, n.ID, now); err != nil {
			t.Fatalf("TouchNode 失败: %v", err)
		}

		got, _ := st.GetNode(ctx, n.ID)
		if got.Status != status {
			t.Errorf("状态 %q 被心跳覆盖成了 %q —— "+
				"机器真关掉后会误报一条掉线告警", status, got.Status)
		}
		// 心跳时间本身还是要更新的，那是「最后一次听到它」的事实。
		if got.LastSeen == nil {
			t.Errorf("状态 %q 下 last_seen 也该更新", status)
		}
	}
}

// 反面：普通状态照常被刷成在线，别把正常路径一起堵死。
func TestTouchNodeStillMarksOnline(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()

	for _, status := range []string{StatusUnknown, StatusOffline, StatusOnline} {
		n := newForwardTestNode(t, st, "节点-"+status)
		st.SetNodeStatus(ctx, n.ID, status)

		if err := st.TouchNode(ctx, n.ID, time.Now().UTC()); err != nil {
			t.Fatalf("TouchNode 失败: %v", err)
		}
		got, _ := st.GetNode(ctx, n.ID)
		if got.Status != StatusOnline {
			t.Errorf("状态 %q 收到心跳后应变成 online，实际 %q", status, got.Status)
		}
	}
}

// 事务内版本必须和非事务版本行为一致 —— 上报路径走的是事务那条。
// 之前两处各写了一遍 SQL，加新状态时只改一处就等于没改。
func TestTxTouchNodeMatchesStoreVersion(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()

	n := newForwardTestNode(t, st, "阿里云香港01")
	st.SetNodeStatus(ctx, n.ID, StatusPlannedStop)

	err := st.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.TouchNode(ctx, n.ID, time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("事务内 TouchNode 失败: %v", err)
	}

	got, _ := st.GetNode(ctx, n.ID)
	if got.Status != StatusPlannedStop {
		t.Errorf("事务内 TouchNode 把 planned_stop 覆盖成了 %q —— "+
			"上报路径走的正是这一条", got.Status)
	}
}
