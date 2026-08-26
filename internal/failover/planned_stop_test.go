package failover

import (
	"testing"

	"github.com/zxcll/vps-panel/internal/store"
)

// 「计划内停机」是这次改动的核心：CDT 按计划把机器停了（定时关机 / 流量熔断），
// 那是面板自己安排的，不是故障。当成故障处理会造成两件不该发生的事 ——
// 发掉线告警、把域名解析切走。
//
// 这一组守的是域名那一半。

func plannedStopState() *NodeState {
	return &NodeState{
		Node: &store.Node{
			ID: 1, Name: "阿里云香港01", IPv4: "8.218.70.0",
			Status: store.StatusPlannedStop, Enabled: true,
		},
	}
}

func plainRecord() *store.DNSRecord {
	return &store.DNSRecord{
		RecordType: "A", SwitchOnExceed: true, SwitchOnOffline: true, Enabled: true,
	}
}

// 默认行为：计划内停机不切走。
func TestPlannedStopStaysEligibleByDefault(t *testing.T) {
	s, rec := plannedStopState(), plainRecord()

	// confirmedDown 传 true —— 机器确实停了，拨测一定不通。
	// 这正是最容易出错的地方：不先拦住 planned_stop 的话，
	// 就会落到 SwitchOnOffline 那条分支被当成故障切走。
	ok, reason := eligible(s, rec, true)
	if !ok {
		t.Fatalf("计划内停机默认不该切走解析，实际被判定为不可用：%s", reason)
	}
}

// 打开开关之后，照常切走。
func TestPlannedStopSwitchesWhenEnabled(t *testing.T) {
	s, rec := plannedStopState(), plainRecord()
	rec.SwitchOnPlannedStop = true

	ok, reason := eligible(s, rec, true)
	if ok {
		t.Fatal("开了「计划内停机也切换」之后应当切走")
	}
	if reason == "" {
		t.Error("不可用时要给出原因，界面上要显示")
	}
}

// 节点被禁用优先级更高：禁用了就是不能用，跟是不是计划内停机无关。
func TestPlannedStopStillRespectsDisabled(t *testing.T) {
	s, rec := plannedStopState(), plainRecord()
	s.Node.Enabled = false

	if ok, _ := eligible(s, rec, true); ok {
		t.Error("节点被禁用时不该因为「计划内停机」而被放行")
	}
}

// 没填 IP 同样不能承接解析 —— 这条检查在 planned_stop 之前，不能被绕过。
func TestPlannedStopStillRequiresIP(t *testing.T) {
	s, rec := plannedStopState(), plainRecord()
	s.Node.IPv4 = ""

	if ok, _ := eligible(s, rec, true); ok {
		t.Error("没有 IP 的节点不该因为「计划内停机」而被放行")
	}
}

// 普通的 stopped（超额动作真把机器关了）行为不变，照常切走。
// 别把两个状态搞混了。
func TestStoppedStillSwitchesAway(t *testing.T) {
	s, rec := plannedStopState(), plainRecord()
	s.Node.Status = store.StatusStopped

	if ok, _ := eligible(s, rec, true); ok {
		t.Error("被面板关停的节点应当切走，这条行为不该被改动")
	}
}
