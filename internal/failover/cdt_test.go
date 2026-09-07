package failover

import (
	"context"
	"testing"

	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/store"
)

type staleCDTProvider struct{ *fakeProvider }

func (p staleCDTProvider) Upsert(_ context.Context, r dns.Record) (dns.Record, error) { return r, nil }

func TestCDTDNSUsesECSAddressAndVerifiesProvider(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{records: []dns.Record{{ID: "rec-1", Zone: "example.com", Name: "us.example.com", Type: "A", Content: "1.1.1.1", TTL: 60}}}
	m, st, r, node := setupApply(t, fp)
	r.Strategy = store.StrategyCDTRotation
	if err := st.UpdateDNSRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.77"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDNSRecord(ctx, r.ID)
	if got.CurrentValue != "198.51.100.77" || got.CurrentNodeID != nil {
		t.Fatalf("must persist ECS address without a node binding: %+v", got)
	}
	if fp.upsertCount() != 1 {
		t.Fatal("DNS not written")
	}
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.77"); err != nil {
		t.Fatal(err)
	}
	if fp.upsertCount() != 1 {
		t.Fatal("unchanged DNS was rewritten")
	}
	if _, err := m.SwitchTo(ctx, r.ID, node.ID); err == nil {
		t.Fatal("manual node switch took over a rotation record")
	}
	if err := m.ReconcileWith(ctx, map[int64]*NodeState{}, store.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if fp.upsertCount() != 1 {
		t.Fatal("ordinary failover touched the rotation record")
	}
	// An accepted write with stale readback must not advance local DNS state.
	cached := m.providers[r.ProviderID]
	cached.p = staleCDTProvider{fp}
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.88"); err == nil {
		t.Fatal("stale readback was accepted")
	}
	got, _ = st.GetDNSRecord(ctx, r.ID)
	if got.CurrentValue != "198.51.100.77" {
		t.Fatal("failed verification changed local DNS state")
	}
}

func TestCDTDNSRejectsDisabledWrongStrategyAndLongTTL(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{}
	m, st, r, _ := setupApply(t, fp)
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.1"); err == nil {
		t.Fatal("accepted ordinary failover record")
	}
	r.Strategy = store.StrategyCDTRotation
	r.TTL = 3600
	st.UpdateDNSRecord(ctx, r)
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.1"); err == nil {
		t.Fatal("accepted TTL beyond grace period")
	}
	r.TTL = 60
	r.Enabled = false
	st.UpdateDNSRecord(ctx, r)
	if err := m.SyncCDTRecord(ctx, r.ID, "198.51.100.1"); err == nil {
		t.Fatal("accepted disabled record")
	}
	if fp.upsertCount() != 0 {
		t.Fatal("validation failures caused a DNS mutation")
	}
}
