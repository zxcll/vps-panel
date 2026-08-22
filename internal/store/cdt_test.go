package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newCDTTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStore(t, filepath.Join(t.TempDir(), "cdt.db"))
}

func newCDTTestAccount(t *testing.T, st *Store, name string) *CDTAccount {
	t.Helper()
	a := &CDTAccount{
		Name: name, AccessKeyID: "LTAI" + name, RegionID: "cn-hongkong",
		SiteType:        CDTSiteInternational,
		QuotaMainlandGB: 20, QuotaOverseasGB: 200,
		ThresholdPercent: 95, ShutdownMode: CDTStopCharging,
		ScheduleTZ: "Asia/Shanghai", Enabled: true,
	}
	if err := st.CreateCDTAccount(context.Background(), a, []byte("密文占位")); err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	return a
}

func TestCDTAccountCRUD(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()

	a := newCDTTestAccount(t, st, "香港号")
	if a.ID == 0 {
		t.Fatal("应回填 ID")
	}

	got, err := st.GetCDTAccount(ctx, a.ID)
	if err != nil {
		t.Fatalf("读账号失败: %v", err)
	}
	if got.Name != "香港号" || got.RegionID != "cn-hongkong" {
		t.Errorf("读回来的账号不对：%+v", got)
	}
	if got.ThresholdPercent != 95 || got.QuotaOverseasGB != 200 {
		t.Errorf("额度/阈值没存对：%+v", got)
	}
	if got.Tripped() {
		t.Error("新建的账号不该是熔断状态")
	}

	// 凭据密文单独取，不出现在账号结构体里 ——
	// CDTAccount 是要整个 JSON 给前端的，密文混进去迟早会漏出去。
	enc, err := st.CDTAccountCred(ctx, a.ID)
	if err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	if string(enc) != "密文占位" {
		t.Errorf("凭据密文对不上：%q", enc)
	}

	got.Name = "改名了"
	got.ThresholdPercent = 80
	if err := st.UpdateCDTAccount(ctx, got, nil); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	after, _ := st.GetCDTAccount(ctx, a.ID)
	if after.Name != "改名了" || after.ThresholdPercent != 80 {
		t.Errorf("更新没生效：%+v", after)
	}

	// credEnc 传 nil 表示不改凭据：前端编辑时不会回填 Secret，
	// 留空必须保持原样，而不是把凭据清空。
	stillEnc, _ := st.CDTAccountCred(ctx, a.ID)
	if string(stillEnc) != "密文占位" {
		t.Errorf("credEnc 传 nil 时不该动凭据，实际变成 %q", stillEnc)
	}

	if err := st.UpdateCDTAccount(ctx, after, []byte("新密文")); err != nil {
		t.Fatalf("换凭据失败: %v", err)
	}
	newEnc, _ := st.CDTAccountCred(ctx, a.ID)
	if string(newEnc) != "新密文" {
		t.Errorf("换凭据没生效：%q", newEnc)
	}

	if err := st.DeleteCDTAccount(ctx, a.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := st.GetCDTAccount(ctx, a.ID); err == nil {
		t.Error("删完还能读到")
	}
}

func TestCDTTrippedMarker(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	if err := st.MarkCDTTripped(ctx, a.ID, now, "2026-08", "中国内地流量跑满"); err != nil {
		t.Fatalf("落熔断标记失败: %v", err)
	}

	got, _ := st.GetCDTAccount(ctx, a.ID)
	if !got.Tripped() {
		t.Fatal("应处于熔断状态")
	}
	if got.TrippedCycle != "2026-08" || got.TrippedReason != "中国内地流量跑满" {
		t.Errorf("熔断信息没存对：%+v", got)
	}

	if err := st.ClearCDTTripped(ctx, a.ID); err != nil {
		t.Fatalf("解除熔断失败: %v", err)
	}
	after, _ := st.GetCDTAccount(ctx, a.ID)
	if after.Tripped() || after.TrippedCycle != "" || after.TrippedReason != "" {
		t.Errorf("解除熔断没清干净：%+v", after)
	}
}

func TestCDTInstanceUpsertKeepsGuarded(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	inst := &CDTInstance{
		AccountID: a.ID, InstanceID: "i-abc", InstanceName: "机器",
		RegionID: "cn-hongkong", Status: "Running", PublicIP: "1.2.3.4",
		InstanceType: "ecs.t6", IsSpot: true, BandwidthMbps: 100,
	}
	if err := st.UpsertCDTInstance(ctx, inst); err != nil {
		t.Fatalf("写实例失败: %v", err)
	}

	list, err := st.CDTInstancesOf(ctx, a.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("应有 1 台实例，实际 %d, err=%v", len(list), err)
	}
	if !list[0].IsSpot || list[0].PublicIP != "1.2.3.4" {
		t.Errorf("实例字段没存对：%+v", list[0])
	}

	// 用户把它标成受守护。
	if err := st.SetCDTInstanceGuarded(ctx, list[0].ID, true); err != nil {
		t.Fatalf("打守护标记失败: %v", err)
	}

	// 再同步一次：状态要更新，但 guarded 是用户设的，同步不能把它冲掉。
	// 冲掉的后果是熔断和保活对这台机器静默失效。
	inst.Status = "Stopped"
	inst.PublicIP = "5.6.7.8"
	if err := st.UpsertCDTInstance(ctx, inst); err != nil {
		t.Fatalf("再次同步失败: %v", err)
	}

	list, _ = st.CDTInstancesOf(ctx, a.ID)
	if len(list) != 1 {
		t.Fatalf("重复同步不该新增行，实际 %d 行", len(list))
	}
	if list[0].Status != "Stopped" || list[0].PublicIP != "5.6.7.8" {
		t.Errorf("同步没更新状态：%+v", list[0])
	}
	if !list[0].Guarded {
		t.Error("同步把用户设的 guarded 标记冲掉了 —— 熔断和保活会对这台机器静默失效")
	}
}

func TestPruneCDTInstances(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	for _, id := range []string{"i-1", "i-2", "i-3"} {
		if err := st.UpsertCDTInstance(ctx, &CDTInstance{
			AccountID: a.ID, InstanceID: id, Status: "Running",
		}); err != nil {
			t.Fatalf("写实例失败: %v", err)
		}
	}

	// i-2 被释放了，本次同步没拉到它。
	if err := st.PruneCDTInstances(ctx, a.ID, map[string]bool{"i-1": true, "i-3": true}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	list, _ := st.CDTInstancesOf(ctx, a.ID)
	if len(list) != 2 {
		t.Fatalf("应剩 2 台，实际 %d", len(list))
	}
	for _, inst := range list {
		if inst.InstanceID == "i-2" {
			t.Error("已释放的实例应被清掉")
		}
	}
}

// 阿里云返回的是「本月至今累计值」，某个地域这个月没流量了就不会出现在结果里。
// 所以必须整体替换：只做 upsert 的话旧记录会一直挂着把用量算高，进而误熔断。
func TestReplaceCDTTrafficDropsStaleRegions(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	first := []CDTTraffic{
		{BusinessRegionID: "cn-hongkong", TrafficType: "BGP", TrafficBytes: 5 << 30},
		{BusinessRegionID: "cn-hangzhou", TrafficType: "BGP", TrafficBytes: 1 << 30},
	}
	if err := st.ReplaceCDTTraffic(ctx, a.ID, "2026-08", first); err != nil {
		t.Fatalf("写流量失败: %v", err)
	}

	got, _ := st.CDTTrafficOf(ctx, a.ID, "2026-08")
	if len(got) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(got))
	}

	// 这次只拉到香港了。
	second := []CDTTraffic{
		{BusinessRegionID: "cn-hongkong", TrafficType: "BGP", TrafficBytes: 6 << 30},
	}
	if err := st.ReplaceCDTTraffic(ctx, a.ID, "2026-08", second); err != nil {
		t.Fatalf("重写流量失败: %v", err)
	}

	got, _ = st.CDTTrafficOf(ctx, a.ID, "2026-08")
	if len(got) != 1 {
		t.Fatalf("杭州那条应被清掉，实际还剩 %d 条：%+v", len(got), got)
	}
	if got[0].BusinessRegionID != "cn-hongkong" || got[0].TrafficBytes != 6<<30 {
		t.Errorf("香港那条没更新：%+v", got[0])
	}
}

// 账期之间不能串：8 月的数据不该影响 9 月。
func TestCDTTrafficIsolatedPerCycle(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	st.ReplaceCDTTraffic(ctx, a.ID, "2026-08", []CDTTraffic{
		{BusinessRegionID: "cn-hongkong", TrafficBytes: 100 << 30},
	})
	st.ReplaceCDTTraffic(ctx, a.ID, "2026-09", []CDTTraffic{
		{BusinessRegionID: "cn-hongkong", TrafficBytes: 1 << 30},
	})

	aug, _ := st.CDTTrafficOf(ctx, a.ID, "2026-08")
	sep, _ := st.CDTTrafficOf(ctx, a.ID, "2026-09")

	if len(aug) != 1 || aug[0].TrafficBytes != 100<<30 {
		t.Errorf("8 月数据被动了：%+v", aug)
	}
	if len(sep) != 1 || sep[0].TrafficBytes != 1<<30 {
		t.Errorf("9 月数据不对：%+v", sep)
	}
}

func TestCDTBillRoundTrip(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	// 还没同步过时返回 nil, nil —— 这不是错误，界面上显示成「—」即可。
	got, err := st.GetCDTBill(ctx, a.ID, "2026-08")
	if err != nil {
		t.Fatalf("读账单不该报错: %v", err)
	}
	if got != nil {
		t.Errorf("还没同步过应返回 nil，实际 %+v", got)
	}

	b := &CDTBill{
		AccountID: a.ID, Cycle: "2026-08",
		AvailableAmount: 12.34, Outstanding: 5.67, Currency: "USD", Symbol: "$",
	}
	if err := st.SaveCDTBill(ctx, b); err != nil {
		t.Fatalf("写账单失败: %v", err)
	}
	// 重复写要覆盖而不是报主键冲突。
	b.Outstanding = 8.90
	if err := st.SaveCDTBill(ctx, b); err != nil {
		t.Fatalf("重写账单失败: %v", err)
	}

	got, _ = st.GetCDTBill(ctx, a.ID, "2026-08")
	if got == nil || got.Outstanding != 8.90 || got.AvailableAmount != 12.34 {
		t.Errorf("账单没存对：%+v", got)
	}
}

// 删账号要把它名下的实例、流量、账单一起带走，别留孤儿数据。
func TestDeleteCDTAccountCascades(t *testing.T) {
	st := newCDTTestStore(t)
	ctx := context.Background()
	a := newCDTTestAccount(t, st, "账号")

	st.UpsertCDTInstance(ctx, &CDTInstance{AccountID: a.ID, InstanceID: "i-1", Status: "Running"})
	st.ReplaceCDTTraffic(ctx, a.ID, "2026-08", []CDTTraffic{
		{BusinessRegionID: "cn-hongkong", TrafficBytes: 1 << 30},
	})
	st.SaveCDTBill(ctx, &CDTBill{AccountID: a.ID, Cycle: "2026-08", Outstanding: 1})

	if err := st.DeleteCDTAccount(ctx, a.ID); err != nil {
		t.Fatalf("删账号失败: %v", err)
	}

	if insts, _ := st.CDTInstancesOf(ctx, a.ID); len(insts) != 0 {
		t.Errorf("实例没被级联删掉，还剩 %d 台", len(insts))
	}
	if tr, _ := st.CDTTrafficOf(ctx, a.ID, "2026-08"); len(tr) != 0 {
		t.Errorf("流量没被级联删掉，还剩 %d 条", len(tr))
	}
	if bill, _ := st.GetCDTBill(ctx, a.ID, "2026-08"); bill != nil {
		t.Error("账单没被级联删掉")
	}
}
