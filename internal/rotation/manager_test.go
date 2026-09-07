package rotation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/cdt"
	"github.com/zxcll/vps-panel/internal/store"
)

type fakeBackend struct {
	st            *store.Store
	now           time.Time
	remote        map[int64]*store.CDTInstance
	used          map[int64]int64
	starts, stops []int64
	dns           []string
	dnsError      error
	quotaError    map[int64]error
	stopError     map[int64]error
}

func (f *fakeBackend) CheckTraffic(ctx context.Context, a *store.CDTAccount) error {
	if err := f.quotaError[a.ID]; err != nil {
		return err
	}
	cycle := string(cdt.CycleOf(f.now))
	if err := f.st.ReplaceCDTTraffic(ctx, a.ID, cycle, []store.CDTTraffic{{BusinessRegionID: "cn-hongkong", TrafficBytes: f.used[a.ID]}}); err != nil {
		return err
	}
	if err := f.st.MarkCDTSynced(ctx, a.ID, f.now, ""); err != nil {
		return err
	}
	status := cdt.EvaluateAt(cdt.SumByBucket(map[string]int64{"cn-hongkong": f.used[a.ID]}), cdt.QuotaFromGB(a.QuotaMainlandGB, a.QuotaOverseasGB), a.ThresholdPercent, f.now)
	if status.Trip && !a.Tripped() {
		return f.st.MarkCDTTripped(ctx, a.ID, f.now, cycle, status.Reason)
	}
	return nil
}
func (f *fakeBackend) RefreshInstance(ctx context.Context, i *store.CDTInstance) (*store.CDTInstance, error) {
	r := f.remote[i.ID]
	if r == nil {
		return nil, store.ErrNotFound
	}
	if err := f.st.UpsertCDTInstance(ctx, r); err != nil {
		return nil, err
	}
	return f.st.GetCDTInstance(ctx, i.ID)
}
func (f *fakeBackend) Start(ctx context.Context, i *store.CDTInstance) error {
	f.starts = append(f.starts, i.ID)
	f.remote[i.ID].Status = alicloud.StatusStarting
	return f.st.SetCDTInstanceStatus(ctx, i.ID, alicloud.StatusStarting)
}
func (f *fakeBackend) Stop(ctx context.Context, i *store.CDTInstance) (bool, error) {
	if err := f.stopError[i.ID]; err != nil {
		return false, err
	}
	r := f.remote[i.ID]
	if r.Status == alicloud.StatusStopped {
		f.st.SetCDTInstanceStatus(ctx, i.ID, r.Status)
		return true, nil
	}
	if r.Status != alicloud.StatusStopping {
		f.stops = append(f.stops, i.ID)
	}
	r.Status = alicloud.StatusStopping
	return false, f.st.SetCDTInstanceStatus(ctx, i.ID, r.Status)
}
func (f *fakeBackend) SyncDNS(ctx context.Context, id int64, ip string) error {
	if f.dnsError != nil {
		return f.dnsError
	}
	f.dns = append(f.dns, ip)
	return nil
}

func fixture(t *testing.T) (*Manager, *fakeBackend, store.CDTRotation) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeBackend{st: st, now: time.Date(2026, 9, 6, 7, 59, 0, 0, time.FixedZone("CST", 8*3600)), remote: map[int64]*store.CDTInstance{}, used: map[int64]int64{}, quotaError: map[int64]error{}, stopError: map[int64]error{}}
	c := store.CDTRotation{Enabled: true, Timezone: "Asia/Shanghai"}
	ctx := context.Background()
	p := &store.DNSProvider{Name: "test", Type: store.ProviderCloudflare, CredEnc: []byte("unused")}
	if err := st.CreateDNSProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := &store.DNSRecord{ProviderID: p.ID, Name: "vps.example.com", Zone: "example.com", RecordType: "A", Strategy: store.StrategyCDTRotation, TTL: 60, Enabled: true}
	if err := st.CreateDNSRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
	c.DNSRecordID = r.ID
	clocks := []string{"00:00", "08:00", "16:00", "00:00"}
	for n, name := range []string{"A", "B", "C"} {
		a := &store.CDTAccount{Name: name, AccessKeyID: name, RegionID: "cn-hongkong", SiteType: store.CDTSiteChina, QuotaMainlandGB: 20, QuotaOverseasGB: 200, ThresholdPercent: 94, ShutdownMode: store.CDTStopKeepCharging, Enabled: true, SyncIntervalSec: 300}
		if err := st.CreateCDTAccount(ctx, a, []byte("unused")); err != nil {
			t.Fatal(err)
		}
		i := &store.CDTInstance{AccountID: a.ID, InstanceID: "i-" + name, RegionID: a.RegionID, Status: alicloud.StatusStopped, PublicIP: "203.0.113." + string(rune('1'+n)), IsSpot: true, Guarded: true}
		if n == 0 {
			i.Status = alicloud.StatusRunning
		}
		if err := st.UpsertCDTInstance(ctx, i); err != nil {
			t.Fatal(err)
		}
		list, err := st.CDTInstancesOf(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		i = list[0]
		f.remote[i.ID] = i
		c.Slots = append(c.Slots, store.CDTRotationSlot{AccountID: a.ID, InstanceID: i.ID, Start: clocks[n], End: clocks[n+1]})
		if err := f.CheckTraffic(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	m := New(st, f, nil)
	m.now = func() time.Time { return f.now }
	if err := m.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	return m, f, c
}

func tick(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, m *Manager) store.CDTRotationState {
	t.Helper()
	v, err := m.st.LoadCDTRotationState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestHandoverWaitsForRunningDNSAndThirtyMinutesAcrossRestart(t *testing.T) {
	m, f, c := fixture(t)
	tick(t, m)
	a, b := c.Slots[0].InstanceID, c.Slots[1].InstanceID
	f.now = f.now.Add(time.Minute)
	tick(t, m)
	if len(f.starts) != 1 || f.starts[0] != b || state(t, m).CurrentInstanceID != a || len(f.stops) != 0 {
		t.Fatal("must start B without touching A or DNS")
	}
	f.now = f.now.Add(15 * time.Second)
	f.remote[b].Status, f.remote[b].PublicIP = alicloud.StatusRunning, "198.51.100.22"
	tick(t, m)
	v := state(t, m)
	deadline := f.now.Add(GracePeriod)
	if v.CurrentInstanceID != b || v.CurrentIP != "198.51.100.22" || !v.PendingStops[a].Equal(deadline) {
		t.Fatalf("handover state: %+v", v)
	}
	// A new Manager uses only persisted state, not an in-memory timer.
	m = New(f.st, f, nil)
	m.now = func() time.Time { return f.now }
	f.now = deadline.Add(-time.Second)
	tick(t, m)
	if len(f.stops) != 0 {
		t.Fatal("stopped before the full grace period")
	}
	f.now = deadline
	tick(t, m)
	if len(f.stops) != 1 || f.stops[0] != a {
		t.Fatal("old instance not stopped at deadline")
	}
	if _, ok := state(t, m).PendingStops[a]; !ok {
		t.Fatal("API acceptance is not stopped confirmation")
	}
	f.remote[a].Status = alicloud.StatusStopped
	f.now = f.now.Add(15 * time.Second)
	tick(t, m)
	if _, ok := state(t, m).PendingStops[a]; ok {
		t.Fatal("confirmed stop should clear pending intent")
	}
}

func TestDNSFailureDoesNotScheduleOrExecuteOldStop(t *testing.T) {
	m, f, c := fixture(t)
	tick(t, m)
	a, b := c.Slots[0].InstanceID, c.Slots[1].InstanceID
	f.now = f.now.Add(time.Minute)
	f.remote[b].Status = alicloud.StatusRunning
	f.dnsError = errors.New("Cloudflare unavailable")
	if err := m.Tick(context.Background()); err == nil {
		t.Fatal("expected DNS failure")
	}
	if state(t, m).CurrentInstanceID != a || len(state(t, m).PendingStops) != 0 || len(f.stops) != 0 {
		t.Fatal("DNS failure must preserve old instance")
	}
	f.dnsError = nil
	tick(t, m)
	deadline := state(t, m).PendingStops[a]
	f.now = deadline.Add(time.Minute)
	f.dnsError = errors.New("verification failed")
	_ = m.Tick(context.Background())
	if len(f.stops) != 0 {
		t.Fatal("do not retire old instance when active DNS cannot be verified")
	}
	f.dnsError = nil
	tick(t, m)
	if len(f.stops) != 1 {
		t.Fatal("must retry when DNS recovers")
	}
}

func TestExhaustionStopsImmediatelyAndSkipsScheduledAccount(t *testing.T) {
	m, f, c := fixture(t)
	tick(t, m)
	a, b := c.Slots[0], c.Slots[1]
	f.used[a.AccountID] = 188 * cdt.GB
	acct, _ := m.st.GetCDTAccount(context.Background(), a.AccountID)
	if err := f.CheckTraffic(context.Background(), acct); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(15 * time.Second)
	tick(t, m)
	if len(f.stops) != 1 || f.stops[0] != a.InstanceID || len(f.starts) != 1 || f.starts[0] != b.InstanceID {
		t.Fatal("exhaustion must stop A immediately and start B before its window")
	}
	f.remote[b.InstanceID].Status = alicloud.StatusRunning
	tick(t, m)
	if state(t, m).CurrentInstanceID != b.InstanceID {
		t.Fatal("exhausted scheduled account still selected")
	}
	// A lower later reading cannot unlatch the same month's protection.
	f.used[a.AccountID] = 0
	tick(t, m)
	if state(t, m).CurrentInstanceID != b.InstanceID {
		t.Fatal("current cycle trip was cleared")
	}
}

func TestAllExhaustedAndNewMonthRestoresOnlyCurrentWindow(t *testing.T) {
	m, f, c := fixture(t)
	ctx := context.Background()
	for _, slot := range c.Slots {
		f.st.MarkCDTTripped(ctx, slot.AccountID, f.now, string(cdt.CycleOf(f.now)), "used up")
		f.remote[slot.InstanceID].Status = alicloud.StatusRunning
	}
	if err := m.Tick(ctx); err == nil {
		t.Fatal("expected no eligible accounts")
	}
	if len(f.dns) != 0 || len(f.starts) != 0 || len(f.stops) != 3 {
		t.Fatal("all exhausted must stop all without DNS selection")
	}
	for _, slot := range c.Slots {
		f.remote[slot.InstanceID].Status = alicloud.StatusStopped
		f.st.SetCDTInstanceStatus(ctx, slot.InstanceID, alicloud.StatusStopped)
	}
	f.now = time.Date(2026, 10, 1, 9, 0, 0, 0, f.now.Location())
	tick(t, m)
	if len(f.starts) != 1 || f.starts[0] != c.Slots[1].InstanceID {
		t.Fatalf("month rollover started wrong instances: %v", f.starts)
	}
	for _, slot := range c.Slots {
		a, _ := f.st.GetCDTAccount(ctx, slot.AccountID)
		if a.Tripped() {
			t.Fatal("fresh new-month quota should restore eligibility")
		}
	}
}

func TestNewMonthQuotaFailureDoesNotRestoreAccount(t *testing.T) {
	m, f, c := fixture(t)
	ctx := context.Background()
	for _, slot := range c.Slots {
		f.st.MarkCDTTripped(ctx, slot.AccountID, f.now, string(cdt.CycleOf(f.now)), "used up")
		f.quotaError[slot.AccountID] = errors.New("quota API offline")
	}
	f.now = f.now.AddDate(0, 1, 0)
	if err := m.Tick(ctx); err == nil {
		t.Fatal("expected fresh-quota error")
	}
	if len(f.starts) != 0 || len(f.dns) != 0 {
		t.Fatal("cannot restore without new-month quota evidence")
	}
}

func TestRetiringAccountReselectedCancelsDeadline(t *testing.T) {
	m, f, c := fixture(t)
	tick(t, m)
	a, b, cc := c.Slots[0], c.Slots[1], c.Slots[2]
	f.now = f.now.Add(time.Minute)
	f.remote[b.InstanceID].Status = alicloud.StatusRunning
	tick(t, m)
	ctx := context.Background()
	f.st.MarkCDTTripped(ctx, b.AccountID, f.now, string(cdt.CycleOf(f.now)), "used up")
	f.st.MarkCDTTripped(ctx, cc.AccountID, f.now, string(cdt.CycleOf(f.now)), "used up")
	f.now = f.now.Add(time.Minute)
	tick(t, m)
	if state(t, m).CurrentInstanceID != a.InstanceID {
		t.Fatal("expected fallback to A")
	}
	if _, ok := state(t, m).PendingStops[a.InstanceID]; ok {
		t.Fatal("reselected active instance retains shutdown deadline")
	}
	f.now = f.now.Add(GracePeriod)
	tick(t, m)
	for _, id := range f.stops {
		if id == a.InstanceID {
			t.Fatal("active fallback stopped")
		}
	}
}

func TestStopFailureRetriesAndDisablingCancelsActions(t *testing.T) {
	m, f, c := fixture(t)
	tick(t, m)
	a, b := c.Slots[0].InstanceID, c.Slots[1].InstanceID
	f.now = f.now.Add(time.Minute)
	f.remote[b].Status = alicloud.StatusRunning
	tick(t, m)
	f.now = f.now.Add(GracePeriod)
	f.stopError[a] = errors.New("temporary stop failure")
	if err := m.Tick(context.Background()); err == nil {
		t.Fatal("expected stop failure")
	}
	if _, ok := state(t, m).PendingStops[a]; !ok {
		t.Fatal("stop failure lost its retry intent")
	}
	delete(f.stopError, a)
	tick(t, m)
	if len(f.stops) != 1 {
		t.Fatal("did not retry stop")
	}
	c.Enabled = false
	if err := m.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	before := len(f.dns)
	tick(t, m)
	if len(f.dns) != before || len(state(t, m).PendingStops) != 0 {
		t.Fatal("disabled plan still acts")
	}
}

func TestPlanBoundariesValidationAndTimezone(t *testing.T) {
	_, _, c := fixture(t)
	for _, tc := range []struct {
		clock string
		index int
	}{{"00:00", 0}, {"07:59", 0}, {"08:00", 1}, {"15:59", 1}, {"16:00", 2}, {"23:59", 2}} {
		at, _ := time.ParseInLocation("2006-01-02 15:04", "2026-09-06 "+tc.clock, time.FixedZone("CST", 8*3600))
		if got := Candidates(c, at.UTC()); len(got) != 3 || got[0].AccountID != c.Slots[tc.index].AccountID {
			t.Fatalf("wrong window at %s", tc.clock)
		}
	}
	c.Slots[0].End = "09:00"
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatal("overlap accepted")
	}
	c.Slots[0].End = "07:00"
	if err := Validate(c); err == nil {
		t.Fatal("gap accepted")
	}
	c.Slots[0].End = "24:00"
	if err := Validate(c); err == nil {
		t.Fatal("invalid clock accepted")
	}
	c.Slots = c.Slots[:1]
	c.Slots[0].Start = "00:00"
	c.Slots[0].End = "00:00"
	if err := Validate(c); err != nil {
		t.Fatal("single full-day window rejected", err)
	}
}
