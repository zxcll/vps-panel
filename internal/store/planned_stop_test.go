package store

import (
	"context"
	"path/filepath"
	"testing"
)

// nodes.cdt_instance_id 和 dns_records.switch_on_planned_stop 都是后加的列。
// SQLite 的 ALTER TABLE ADD COLUMN 没有 IF NOT EXISTS，而本项目的迁移每次
// 启动全量重放 —— 直接写进 .sql 就是第二次启动起不来（这个坑踩过一次）。
// 所以走 PRAGMA table_info 检查后再补，这条用例反复开关库确认它幂等。
func TestPlannedStopColumnsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	st := openTestStore(t, path)
	n := newForwardTestNode(t, st, "阿里云香港01")
	n.CDTInstanceID = 42
	n.IPv4 = "8.218.70.0"
	if err := st.UpdateNode(ctx, n); err != nil {
		t.Fatalf("设置 CDT 关联失败: %v", err)
	}

	prov := &DNSProvider{Name: "测试服务商", Type: ProviderCloudflare, CredEnc: []byte("x")}
	if err := st.CreateDNSProvider(ctx, prov); err != nil {
		t.Fatalf("建服务商失败: %v", err)
	}

	rec := &DNSRecord{
		ProviderID: prov.ID,
		Zone:       "example.com", Name: "hk.example.com", RecordType: "A", TTL: 600,
		Strategy: StrategyFailover, SwitchOnOffline: true,
		SwitchOnPlannedStop: true, Enabled: true,
	}
	if err := st.CreateDNSRecord(ctx, rec); err != nil {
		t.Fatalf("建 DNS 记录失败: %v", err)
	}
	st.Close()

	// 重放三次：第一次是「升级后第一次启动」，后面才是真正的考验。
	for i := range 3 {
		again, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次重放迁移失败（面板起不来就是这个错）: %v", i+1, err)
		}

		got, err := again.GetNode(ctx, n.ID)
		if err != nil {
			again.Close()
			t.Fatalf("第 %d 次重放后读节点失败: %v", i+1, err)
		}
		if got.CDTInstanceID != 42 {
			again.Close()
			t.Fatalf("第 %d 次重放后 CDT 关联变成了 %d，应保持 42", i+1, got.CDTInstanceID)
		}

		recs, err := again.ListDNSRecords(ctx)
		if err != nil || len(recs) != 1 {
			again.Close()
			t.Fatalf("第 %d 次重放后读 DNS 记录失败: %v", i+1, err)
		}
		if !recs[0].SwitchOnPlannedStop {
			again.Close()
			t.Fatalf("第 %d 次重放后 switch_on_planned_stop 被冲掉了", i+1)
		}
		again.Close()
	}
}

// 关联字段默认是 0（未关联），别让新建的节点莫名其妙挂上一个实例。
func TestNodeCDTLinkDefaultsToUnlinked(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()

	n := newForwardTestNode(t, st, "普通节点")
	got, err := st.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("读节点失败: %v", err)
	}
	if got.CDTInstanceID != 0 {
		t.Errorf("新建节点不该有 CDT 关联，实际 %d", got.CDTInstanceID)
	}
}

// 计划内停机是个独立状态，不能和 stopped 混为一谈 ——
// 两者在域名切换和告警上的处理完全相反。
func TestPlannedStopStatusRoundTrips(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()

	n := newForwardTestNode(t, st, "阿里云香港01")
	if err := st.SetNodeStatus(ctx, n.ID, StatusPlannedStop); err != nil {
		t.Fatalf("设置状态失败: %v", err)
	}
	got, _ := st.GetNode(ctx, n.ID)
	if got.Status != StatusPlannedStop {
		t.Errorf("状态应是 %q，实际 %q", StatusPlannedStop, got.Status)
	}
	if got.Status == StatusStopped {
		t.Error("planned_stop 不该等于 stopped")
	}
}
