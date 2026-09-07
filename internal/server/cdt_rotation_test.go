package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/cdt"
	"github.com/zxcll/vps-panel/internal/cdtctl"
	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/store"
)

type rotationTransport func(*http.Request) (*http.Response, error)

func (f rotationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rotationServer(t *testing.T) (*Server, *store.CDTAccount, *store.CDTInstance) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cipher, err := crypto.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cred, err := cipher.Encrypt("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	a := &store.CDTAccount{Name: "A", AccessKeyID: "test-key", RegionID: "cn-hongkong", Enabled: true, KeepAlive: true, ShutdownMode: store.CDTStopKeepCharging, ScheduleTZ: "Asia/Shanghai"}
	ctx := context.Background()
	if err := st.CreateCDTAccount(ctx, a, cred); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCDTInstance(ctx, &store.CDTInstance{AccountID: a.ID, InstanceID: "i-test", RegionID: a.RegionID, Guarded: true, IsSpot: true, Status: alicloud.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	insts, _ := st.CDTInstancesOf(ctx, a.ID)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{st: st, cipher: cipher, cdt: newCDTGuard(), log: log, cdtCtl: cdtctl.New(st, cipher, nil, log)}
	return s, a, insts[0]
}

func TestRotationStopAlwaysRequestsSavingsAndWaitsForStopped(t *testing.T) {
	s, _, i := rotationServer(t)
	ctx := context.Background()
	remoteStatus := "Running"
	stopCalls := 0
	old := http.DefaultTransport
	http.DefaultTransport = rotationTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		body := `{"RequestId":"test"}`
		switch r.Form.Get("Action") {
		case "DescribeInstanceStatus":
			body = `{"InstanceStatuses":{"InstanceStatus":[{"InstanceId":"i-test","Status":"` + remoteStatus + `"}]}}`
		case "StopInstance":
			stopCalls++
			if r.Form.Get("StoppedMode") != "StopCharging" || r.Form.Get("ForceStop") != "false" {
				t.Fatal("rotation must request non-forced savings shutdown")
			}
		default:
			t.Fatalf("unexpected cloud action %s", r.Form.Get("Action"))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	b := rotationBackend{s}
	done, err := b.Stop(ctx, i)
	if err != nil || done || stopCalls != 1 {
		t.Fatalf("accepted stop must remain pending: %v %v %d", done, err, stopCalls)
	}
	stored, _ := s.st.GetCDTInstance(ctx, i.ID)
	if !stored.PlannedStop || stored.Status != alicloud.StatusStopping {
		t.Fatal("stop intent not persisted")
	}
	// The eventual-consistency Running response must not cause a duplicate stop.
	done, err = b.Stop(ctx, stored)
	if err != nil || done || stopCalls != 1 {
		t.Fatal("duplicate stop during status lag")
	}
	remoteStatus = "Stopped"
	done, err = b.Stop(ctx, stored)
	if err != nil || !done || stopCalls != 1 {
		t.Fatal("stopped state not confirmed")
	}
}

func TestRotationOwnershipBlocksLegacyAndManualPower(t *testing.T) {
	s, a, i := rotationServer(t)
	ctx := context.Background()
	s.st.SaveCDTRotation(ctx, store.CDTRotation{Enabled: true, Timezone: "Asia/Shanghai", Slots: []store.CDTRotationSlot{{AccountID: a.ID, InstanceID: i.ID, Start: "00:00", End: "00:00"}}})
	a.TrippedAt = time.Now()
	a.TrippedCycle = "2020-01"
	s.st.MarkCDTTripped(ctx, a.ID, a.TrippedAt, a.TrippedCycle, "old trip")
	// No cloud transport is needed: all of these must exit before client creation.
	s.cdtRollCycle(ctx, a)
	s.cdtScheduledPower(ctx, a, true)
	s.cdtScheduledPower(ctx, a, false)
	a.TrippedAt = time.Time{}
	s.cdtKeepAlive(ctx, a)
	stored, _ := s.st.GetCDTAccount(ctx, a.ID)
	if !stored.Tripped() {
		t.Fatal("legacy month rollover cleared a managed trip")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/cdt/instances/1/start", strings.NewReader(`{}`))
	r.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	s.cdtPower(w, r, true)
	if w.Code != http.StatusConflict {
		t.Fatalf("manual start status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestScheduledStartCannotClearCurrentCycleTrip(t *testing.T) {
	s, a, _ := rotationServer(t)
	a.TrippedAt = time.Now()
	a.TrippedCycle = string(cdt.CurrentCycle())
	if err := s.st.MarkCDTTripped(context.Background(), a.ID, a.TrippedAt, a.TrippedCycle, "quota exhausted"); err != nil {
		t.Fatal(err)
	}
	s.cdtScheduledPower(context.Background(), a, true)
	stored, _ := s.st.GetCDTAccount(context.Background(), a.ID)
	if !stored.Tripped() {
		t.Fatal("scheduled start bypassed monthly protection")
	}
}
