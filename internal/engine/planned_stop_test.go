package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/failover"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/store"
)

// 这一组复现用户报的第一个 bug：「关联了 CDT，定时关机了还是会提示离线通知」。
//
// 时序是这样的：
//   1. 定时关机 → 节点标成 planned_stop
//   2. 机器执行关机要几十秒，这期间探针**还在上报**
//   3. 老代码允许「上线方向」把 planned_stop 翻回 online
//   4. 机器真关掉 → 状态已经是 online → 落进离线分支 → 发掉线告警
//
// 所以 planned_stop 必须是面板独占的：探针心跳两个方向都不能动它。

func newEngineForTest(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 通知器指向同一个库；没配 Telegram/Webhook 时 Send 是空转，
	// 所以这里只需要它不 panic。真正的断言看事件表。
	return &Engine{st: st, notifier: notify.New(st, log), log: log,
		working: map[int64]bool{}}, st
}

func plannedStopNode(t *testing.T, st *store.Store) *store.Node {
	t.Helper()
	ctx := context.Background()
	n := &store.Node{
		Name: "阿里云香港01", Secret: "s1", IPv4: "8.218.70.0",
		BillingMode: store.BillingSum, TrafficRatio: 1, WarnPercent: 90,
		ResetDay: 1, ResetTZ: "UTC", Status: store.StatusUnknown,
		Enabled: true, SSHPort: 22,
	}
	if err := st.CreateNode(ctx, n); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	if err := st.SetNodeStatus(ctx, n.ID, store.StatusPlannedStop); err != nil {
		t.Fatalf("置计划内停机失败: %v", err)
	}
	got, _ := st.GetNode(ctx, n.ID)
	return got
}

func offlineEventCount(t *testing.T, st *store.Store) int {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), store.EventFilter{Limit: 100})
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == store.EventNodeOffline {
			n++
		}
	}
	return n
}

// 关机指令发出后、机器真正断电前，探针还在上报 —— 这时候不能把状态翻回 online。
func TestPlannedStopNotFlippedByLingeringHeartbeat(t *testing.T) {
	e, st := newEngineForTest(t)
	ctx := context.Background()
	n := plannedStopNode(t, st)

	// 探针还在上报（机器正在关机的那几十秒）。
	e.syncLiveness(ctx, &failover.NodeState{
		Node: n, AgentOnline: true, HeartbeatFresh: true, Alive: true,
	}, store.Settings{NotifyNodeOnline: true})

	got, _ := st.GetNode(ctx, n.ID)
	if got.Status != store.StatusPlannedStop {
		t.Fatalf("探针的残余心跳把状态翻成了 %q —— 机器真关掉后就会误发掉线告警", got.Status)
	}
}

// 机器真的关掉、心跳断了之后，不能发掉线告警。
func TestPlannedStopDoesNotFireOfflineAlert(t *testing.T) {
	e, st := newEngineForTest(t)
	ctx := context.Background()
	n := plannedStopNode(t, st)

	// 先让它「上报过」，否则会走 LastSeen == nil 那条待接入分支。
	st.TouchNode(ctx, n.ID, time.Now().UTC())
	st.SetNodeStatus(ctx, n.ID, store.StatusPlannedStop)
	n, _ = st.GetNode(ctx, n.ID)

	// 机器关掉了，心跳没了。
	e.syncLiveness(ctx, &failover.NodeState{
		Node: n, AgentOnline: false, HeartbeatFresh: false, Alive: false,
		Reason: "探针失联",
	}, store.Settings{NotifyNodeOnline: true})

	got, _ := st.GetNode(ctx, n.ID)
	if got.Status == store.StatusOffline {
		t.Error("计划内停机的节点不该被翻成 offline")
	}
	if c := offlineEventCount(t, st); c != 0 {
		t.Errorf("计划内停机不该产生掉线事件，实际 %d 条 —— 这正是用户报的 bug", c)
	}
}

// 反面：普通节点掉线照常告警，别把这条路一起堵死了。
func TestNormalNodeStillAlertsOnOffline(t *testing.T) {
	e, st := newEngineForTest(t)
	ctx := context.Background()

	n := plannedStopNode(t, st)
	// 先让它正常在线过一次，拿到 LastSeen。
	st.SetNodeStatus(ctx, n.ID, store.StatusOnline)
	st.TouchNode(ctx, n.ID, time.Now().UTC())
	n, _ = st.GetNode(ctx, n.ID)

	e.syncLiveness(ctx, &failover.NodeState{
		Node: n, AgentOnline: false, HeartbeatFresh: false, Alive: false,
		Reason: "探针失联",
	}, store.Settings{})

	got, _ := st.GetNode(ctx, n.ID)
	if got.Status != store.StatusOffline {
		t.Fatalf("普通节点掉线应翻成 offline，实际 %q", got.Status)
	}
	if c := offlineEventCount(t, st); c != 1 {
		t.Errorf("普通节点掉线应产生 1 条掉线事件，实际 %d —— 别把正常告警一起堵死了", c)
	}
}
