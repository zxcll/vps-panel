package engine

import (
	"strings"
	"testing"

	"github.com/zxcll/vps-panel/internal/store"
)

// isManagedDown 决定「这个状态下机器不在跑，但面板知道为什么」。
// 判错的后果很直接：要么该告警的不告警，要么计划内停机被当成事故刷告警。
func TestIsManagedDown(t *testing.T) {
	managed := []string{
		store.StatusExceeded,    // 流量超额
		store.StatusStopped,     // 超额动作真把机器关了
		store.StatusPlannedStop, // CDT 按计划停的
	}
	for _, st := range managed {
		if !isManagedDown(st) {
			t.Errorf("%q 应算作「面板知道它为什么停」", st)
		}
	}

	// 这几个必须**不**算 —— 算进去的话真掉线就再也不告警了。
	for _, st := range []string{store.StatusOnline, store.StatusOffline, store.StatusUnknown} {
		if isManagedDown(st) {
			t.Errorf("%q 不该算作 managed down，否则真掉线时不会告警", st)
		}
	}
}

// 几种「上线」的含义完全不同，文案不能混用 ——
// 用户看到通知得能分清刚才到底发生了什么。
func TestOnlineMessageDistinguishesCases(t *testing.T) {
	cases := []struct {
		from     string
		wantIn   string
		wantKind string
	}{
		{store.StatusOffline, "恢复在线", "节点已恢复"},
		{store.StatusPlannedStop, "计划内停机中恢复", "节点已开机"},
		{store.StatusExceeded, "流量超额", "节点已恢复"},
		{store.StatusStopped, "已关停", "节点已恢复"},
		{store.StatusUnknown, "探针已接入", "探针已接入"},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		n := &store.Node{Name: "测试机", Status: tc.from}
		event, title, body := onlineMessage(n)

		if !strings.Contains(event, tc.wantIn) {
			t.Errorf("从 %q 上线的事件文案应含 %q，实际 %q", tc.from, tc.wantIn, event)
		}
		if title != tc.wantKind {
			t.Errorf("从 %q 上线的通知标题应是 %q，实际 %q", tc.from, tc.wantKind, title)
		}
		if body == "" {
			t.Errorf("从 %q 上线的通知正文不能为空", tc.from)
		}
		seen[event] = true
	}

	// 五种来源应该产出五种不同的说法，别写成同一句。
	if len(seen) != len(cases) {
		t.Errorf("%d 种上线情形只产出了 %d 种文案，说明有几种被写成一样的了",
			len(cases), len(seen))
	}
}

// 计划内停机恢复时，说的是「已开机」而不是「已恢复」——
// 后者听着像出过故障，而那本来就是计划内的。
func TestPlannedStopRecoveryReadsAsPlanned(t *testing.T) {
	n := &store.Node{Name: "阿里云香港01", Status: store.StatusPlannedStop}
	_, title, body := onlineMessage(n)

	if strings.Contains(title, "恢复") {
		t.Errorf("计划内停机醒来不该说成「恢复」，那听着像出过故障：%q", title)
	}
	if !strings.Contains(body, "计划内停机") {
		t.Errorf("正文应说明是从计划内停机里出来的，实际 %q", body)
	}
}

func TestStatusLabel(t *testing.T) {
	cases := map[string]string{
		store.StatusExceeded:    "流量超额",
		store.StatusStopped:     "已关停",
		store.StatusPlannedStop: "计划内停机",
	}
	for st, want := range cases {
		if got := statusLabel(st); got != want {
			t.Errorf("statusLabel(%q) = %q，期望 %q", st, got, want)
		}
	}
}
