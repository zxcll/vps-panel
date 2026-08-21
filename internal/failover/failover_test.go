package failover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/quota"
	"github.com/zxcll/vps-panel/internal/store"
)

const gb = int64(1) << 30

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key := make([]byte, 32)
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	log := testLogger()
	return New(st, c, notify.New(st, log), nil, log), st
}

func state(id int64, name, ip string, alive bool, q quota.Status) *NodeState {
	return &NodeState{
		Node: &store.Node{
			ID: id, Name: name, IPv4: ip, Enabled: true,
			Status: store.StatusOnline, BillingMode: store.BillingSum, TrafficRatio: 1,
		},
		Alive: alive,
		Quota: q,
	}
}

func record(members ...store.DNSMember) *store.DNSRecord {
	return &store.DNSRecord{
		ID: 1, Zone: "example.com", Name: "us.example.com", RecordType: "A", TTL: 60,
		Strategy:        store.StrategyFailover,
		SwitchOnExceed:  true,
		SwitchOnOffline: true,
		Enabled:         true,
		Members:         members,
	}
}

func TestDecidePicksHighestPriorityHealthyNode(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()
	cfg.FailThreshold = 1

	states := map[int64]*NodeState{
		1: state(1, "主节点", "1.1.1.1", true, quota.Status{}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	rec := record(
		store.DNSMember{NodeID: 2, Priority: 20, NodeName: "备节点"},
		store.DNSMember{NodeID: 1, Priority: 10, NodeName: "主节点"},
	)

	d := m.Decide(rec, states, cfg)
	if d.TargetNodeID != 1 {
		t.Errorf("应选中优先级最高的节点 1，实际 %d", d.TargetNodeID)
	}
	if d.TargetValue != "1.1.1.1" {
		t.Errorf("目标解析值 = %q，期望 1.1.1.1", d.TargetValue)
	}
	if !d.Change {
		t.Error("当前解析值为空，应判定需要变更")
	}
}

func TestDecideSkipsExceededNode(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{
		1: state(1, "主节点", "1.1.1.1", true, quota.Status{Exceeded: true, Billed: 200 * gb, Quota: 100 * gb}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)

	d := m.Decide(rec, states, cfg)
	if d.TargetNodeID != 2 {
		t.Errorf("主节点流量超额，应切到备节点 2，实际 %d", d.TargetNodeID)
	}
	if d.Candidates[0].Eligible {
		t.Error("超额节点不该被判定为可用")
	}
	if d.Candidates[0].Reason != "流量已超额" {
		t.Errorf("淘汰原因 = %q", d.Candidates[0].Reason)
	}
}

// 关掉"超额时切换"开关后，超额节点仍可继续承接解析。
func TestDecideRespectsSwitchOnExceedToggle(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{
		1: state(1, "主节点", "1.1.1.1", true, quota.Status{Exceeded: true}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)
	rec.SwitchOnExceed = false

	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 1 {
		t.Errorf("开关关闭时不该因超额切走，实际选中 %d", d.TargetNodeID)
	}
}

func TestDecideWarningTriggersEarlySwitch(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{
		1: state(1, "主节点", "1.1.1.1", true, quota.Status{Warning: true, Percent: 92}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)
	rec.SwitchOnWarn = true

	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 2 {
		t.Errorf("开启预警切换后应提前切到备节点，实际 %d", d.TargetNodeID)
	}

	// 关掉开关就不该提前切
	rec.SwitchOnWarn = false
	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 1 {
		t.Errorf("未开启预警切换时不该切走，实际 %d", d.TargetNodeID)
	}
}

// 掉线要连续失败达到阈值才切，一次抖动不算。
func TestDecideRequiresFailThreshold(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()
	cfg.FailThreshold = 3

	states := map[int64]*NodeState{
		1: state(1, "主节点", "1.1.1.1", false, quota.Status{}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	states[1].Reason = "探针失联，且端口拨测不可达"
	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)

	// 前两次失败：还不切
	for i := 1; i <= 2; i++ {
		m.MarkAlive(1, false, cfg.FailThreshold)
		if d := m.Decide(rec, states, cfg); d.TargetNodeID != 1 {
			t.Fatalf("第 %d 次失败就切走了（阈值 3），实际选中 %d", i, d.TargetNodeID)
		}
	}

	// 第三次失败：确认下线，切走
	m.MarkAlive(1, false, cfg.FailThreshold)
	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 2 {
		t.Errorf("达到失败阈值后应切到备节点，实际 %d", d.TargetNodeID)
	}

	// 恢复在线后计数清零
	m.MarkAlive(1, true, cfg.FailThreshold)
	if got := m.FailCount(1); got != 0 {
		t.Errorf("恢复在线后失败计数应清零，实际 %d", got)
	}
	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 1 {
		t.Errorf("节点恢复后应切回主节点，实际 %d", d.TargetNodeID)
	}
}

// 所有候选都不可用时，保持现状不动 —— 乱切只会让排查更困难。
func TestDecideKeepsCurrentWhenAllUnhealthy(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()
	cfg.FailThreshold = 1

	states := map[int64]*NodeState{
		1: state(1, "节点一", "1.1.1.1", false, quota.Status{}),
		2: state(2, "节点二", "2.2.2.2", false, quota.Status{}),
	}
	m.MarkAlive(1, false, 1)
	m.MarkAlive(2, false, 1)

	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)
	rec.CurrentValue = "1.1.1.1"

	d := m.Decide(rec, states, cfg)
	if d.Change {
		t.Error("全员不可用时不该改动解析")
	}
	if d.TargetNodeID != 0 {
		t.Errorf("不该选出目标节点，实际 %d", d.TargetNodeID)
	}
	if d.Skipped == "" {
		t.Error("应说明跳过原因")
	}
}

func TestDecideSkipsNodeWithoutIP(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{
		1: state(1, "无 IP", "", true, quota.Status{}),
		2: state(2, "备节点", "2.2.2.2", true, quota.Status{}),
	}
	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
	)

	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 2 {
		t.Errorf("没填 IP 的节点无法作为解析目标，实际选中 %d", d.TargetNodeID)
	}
}

func TestDecideSkipsDisabledAndStoppedNodes(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{
		1: state(1, "已禁用", "1.1.1.1", true, quota.Status{}),
		2: state(2, "已关停", "2.2.2.2", true, quota.Status{}),
		3: state(3, "正常", "3.3.3.3", true, quota.Status{}),
	}
	states[1].Node.Enabled = false
	states[2].Node.Status = store.StatusStopped

	rec := record(
		store.DNSMember{NodeID: 1, Priority: 10},
		store.DNSMember{NodeID: 2, Priority: 20},
		store.DNSMember{NodeID: 3, Priority: 30},
	)

	if d := m.Decide(rec, states, cfg); d.TargetNodeID != 3 {
		t.Errorf("应跳过禁用和关停的节点，实际选中 %d", d.TargetNodeID)
	}
}

func TestDecideNoChangeWhenAlreadyOnTarget(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	states := map[int64]*NodeState{1: state(1, "主节点", "1.1.1.1", true, quota.Status{})}
	rec := record(store.DNSMember{NodeID: 1, Priority: 10})
	rec.CurrentValue = "1.1.1.1"

	if d := m.Decide(rec, states, cfg); d.Change {
		t.Error("已经指向目标节点时不该重复切换")
	}
}

func TestDecideAAAAUsesIPv6(t *testing.T) {
	m, _ := newManager(t)
	cfg := store.DefaultSettings()

	s := state(1, "节点", "1.1.1.1", true, quota.Status{})
	s.Node.IPv6 = "2001:db8::1"
	states := map[int64]*NodeState{1: s}

	rec := record(store.DNSMember{NodeID: 1, Priority: 10})
	rec.RecordType = "AAAA"

	if d := m.Decide(rec, states, cfg); d.TargetValue != "2001:db8::1" {
		t.Errorf("AAAA 记录应使用 IPv6，实际 %q", d.TargetValue)
	}
}

// --- Apply：带假的 DNS 服务商 ---

type fakeProvider struct {
	mu      sync.Mutex
	records []dns.Record
	upserts []dns.Record
	listErr error
	putErr  error
}

func (f *fakeProvider) Type() string { return "fake" }

func (f *fakeProvider) List(ctx context.Context, zone, name, rtype string) ([]dns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]dns.Record(nil), f.records...), nil
}

func (f *fakeProvider) Upsert(ctx context.Context, r dns.Record) (dns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return r, f.putErr
	}
	if r.ID == "" {
		r.ID = "new-1"
	}
	f.upserts = append(f.upserts, r)
	f.records = []dns.Record{r}
	return r, nil
}

func (f *fakeProvider) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

// setupApply 建好真实的库记录（服务商 + 记录组 + 一个候选节点），
// 并把假服务商塞进 Manager 的缓存，避免真实网络调用。
func setupApply(t *testing.T, fp *fakeProvider) (*Manager, *store.Store, *store.DNSRecord, *store.Node) {
	t.Helper()
	m, st := newManager(t)
	ctx := context.Background()

	node := &store.Node{
		Name: "备节点", Secret: "s-" + t.Name(), IPv4: "2.2.2.2",
		BillingMode: store.BillingSum, TrafficRatio: 1, ResetDay: 1, ResetTZ: "UTC",
		Status: store.StatusOnline, Enabled: true, SSHPort: 22,
	}
	if err := st.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	prov := &store.DNSProvider{Name: "测试服务商", Type: "cloudflare"}
	if err := st.CreateDNSProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	// 直接注入缓存，绕开真实的凭据解密与网络调用。
	// 缓存命中要求 updated 与库里的值完全相等，而库里存的是秒级时间戳，
	// 所以这里必须用回读的值，不能用内存里带纳秒的那个。
	stored, err := st.GetDNSProvider(ctx, prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.providers[prov.ID] = &cachedProvider{p: fp, updated: stored.UpdatedAt}

	rec := &store.DNSRecord{
		ProviderID: prov.ID, Zone: "example.com", Name: "us.example.com",
		RecordType: "A", TTL: 60, Strategy: store.StrategyFailover,
		SwitchOnExceed: true, SwitchOnOffline: true, Enabled: true,
	}
	if err := st.CreateDNSRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return m, st, rec, node
}

func TestApplyWritesRecordAndPersists(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{records: []dns.Record{
		{ID: "rec-1", Zone: "example.com", Name: "us.example.com", Type: "A", Content: "1.1.1.1"},
	}}
	m, st, rec, node := setupApply(t, fp)

	d := Decision{
		RecordID: rec.ID, RecordName: rec.Name,
		TargetNodeID: node.ID, TargetNodeName: node.Name, TargetValue: "2.2.2.2",
		CurrentValue: "1.1.1.1", Change: true,
	}
	if err := m.Apply(ctx, rec, d, false, store.DefaultSettings()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if fp.upsertCount() != 1 {
		t.Fatalf("应调用一次 Upsert，实际 %d 次", fp.upsertCount())
	}
	if fp.upserts[0].ID != "rec-1" {
		t.Errorf("应复用已有记录 ID，实际 %q", fp.upserts[0].ID)
	}
	if fp.upserts[0].Content != "2.2.2.2" {
		t.Errorf("提交的解析值 = %q", fp.upserts[0].Content)
	}

	got, err := st.GetDNSRecord(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentValue != "2.2.2.2" {
		t.Errorf("库里记录的当前值 = %q", got.CurrentValue)
	}
	if got.CurrentNodeID == nil || *got.CurrentNodeID != node.ID {
		t.Errorf("库里记录的当前节点 = %v", got.CurrentNodeID)
	}
	if got.LastSwitchAt == nil {
		t.Error("应记录切换时间")
	}
}

// 服务商侧已经是目标值时不该重复写，只补面板侧的记账。
func TestApplySkipsAPICallWhenAlreadyCorrect(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{records: []dns.Record{
		{ID: "rec-1", Name: "us.example.com", Type: "A", Content: "2.2.2.2"},
	}}
	m, st, rec, node := setupApply(t, fp)

	d := Decision{
		RecordID: rec.ID, TargetNodeID: node.ID, TargetValue: "2.2.2.2",
		CurrentValue: "", Change: true,
	}
	if err := m.Apply(ctx, rec, d, false, store.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if fp.upsertCount() != 0 {
		t.Errorf("解析值已正确时不该调用写接口，实际调用 %d 次", fp.upsertCount())
	}

	got, _ := st.GetDNSRecord(ctx, rec.ID)
	if got.CurrentValue != "2.2.2.2" {
		t.Errorf("面板侧记账未补上，当前值 = %q", got.CurrentValue)
	}
}

func TestApplyRespectsCooldown(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{}
	m, _, rec, node := setupApply(t, fp)

	recent := time.Now().UTC().Add(-30 * time.Second)
	rec.LastSwitchAt = &recent

	cfg := store.DefaultSettings()
	cfg.SwitchCooldownSec = 300

	d := Decision{RecordID: rec.ID, TargetNodeID: node.ID, TargetValue: "2.2.2.2", Change: true}
	if err := m.Apply(ctx, rec, d, false, cfg); err != nil {
		t.Fatal(err)
	}
	if fp.upsertCount() != 0 {
		t.Errorf("冷却期内不该执行自动切换，实际调用 %d 次", fp.upsertCount())
	}

	// 手动切换无视冷却期
	if err := m.Apply(ctx, rec, d, true, cfg); err != nil {
		t.Fatal(err)
	}
	if fp.upsertCount() != 1 {
		t.Errorf("手动切换应忽略冷却期，实际调用 %d 次", fp.upsertCount())
	}
}

func TestApplyRecordsErrorOnFailure(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{putErr: errors.New("API Token 权限不足")}
	m, st, rec, node := setupApply(t, fp)

	d := Decision{RecordID: rec.ID, TargetNodeID: node.ID, TargetValue: "2.2.2.2", Change: true}
	err := m.Apply(ctx, rec, d, false, store.DefaultSettings())
	if err == nil {
		t.Fatal("服务商报错时 Apply 应返回错误")
	}

	got, _ := st.GetDNSRecord(ctx, rec.ID)
	if got.LastError == "" {
		t.Error("失败原因应写进记录，便于面板展示")
	}
	if got.CurrentValue == "2.2.2.2" {
		t.Error("切换失败时不该更新当前值")
	}

	events, _ := st.ListEvents(ctx, store.EventFilter{Level: store.LevelError})
	if len(events) == 0 {
		t.Error("切换失败应产生 error 级事件")
	}
}

func TestApplyNoTargetIsNoop(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{}
	m, _, rec, _ := setupApply(t, fp)

	d := Decision{RecordID: rec.ID, TargetNodeID: 0, Skipped: "没有可用节点"}
	if err := m.Apply(ctx, rec, d, false, store.DefaultSettings()); err != nil {
		t.Fatalf("无目标节点时应静默返回，实际报错: %v", err)
	}
	if fp.upsertCount() != 0 {
		t.Error("无目标节点时不该调用服务商接口")
	}
}

func TestMarkAliveThresholdBoundaries(t *testing.T) {
	m, _ := newManager(t)

	// 阈值为 0 或负数时按 1 处理，避免除零式的逻辑错误
	if !m.MarkAlive(1, false, 0) {
		t.Error("阈值 <= 0 应按 1 处理，首次失败即确认下线")
	}
	m.MarkAlive(1, true, 0)

	if m.MarkAlive(2, false, 2) {
		t.Error("阈值 2 时第一次失败不该确认下线")
	}
	if !m.MarkAlive(2, false, 2) {
		t.Error("阈值 2 时第二次失败应确认下线")
	}
	// 持续失败保持确认状态
	if !m.MarkAlive(2, false, 2) {
		t.Error("持续失败应保持已确认下线")
	}
}
