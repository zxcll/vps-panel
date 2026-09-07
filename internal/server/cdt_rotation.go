package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/store"
)

// The rotation manager shares the existing per-account power-operation locks.
type rotationBackend struct{ s *Server }

func (b rotationBackend) CheckTraffic(ctx context.Context, a *store.CDTAccount) error {
	return b.s.cdtCheckTraffic(ctx, a)
}
func (b rotationBackend) SyncDNS(ctx context.Context, id int64, ip string) error {
	return b.s.fo.SyncCDTRecord(ctx, id, ip)
}
func (b rotationBackend) RefreshInstance(ctx context.Context, i *store.CDTInstance) (*store.CDTInstance, error) {
	a, err := b.s.st.GetCDTAccount(ctx, i.AccountID)
	if err != nil {
		return nil, err
	}
	if err := b.s.cdtSyncInstances(ctx, a); err != nil {
		return nil, err
	}
	fresh, err := b.s.st.GetCDTInstance(ctx, i.ID)
	if err == nil && fresh.Status == alicloud.StatusRunning {
		b.s.cdtCtl.ClearNodePlannedStop(ctx, i.ID)
	}
	return fresh, err
}
func (b rotationBackend) Start(ctx context.Context, i *store.CDTInstance) error {
	s := b.s
	if !s.cdtLock(i.AccountID) {
		return fmt.Errorf("账号正在执行其他操作，稍后重试开机")
	}
	defer s.cdtUnlock(i.AccountID)
	a, err := s.st.GetCDTAccount(ctx, i.AccountID)
	if err != nil {
		return err
	}
	if !a.Enabled || a.Tripped() {
		return fmt.Errorf("账号已停用或流量已熔断，禁止换班开机")
	}
	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		return err
	}
	if err := client.StartInstance(ctx, i.InstanceID); err != nil {
		return fmt.Errorf("换班开机 %s：%w", i.InstanceID, err)
	}
	if err := s.st.SetCDTInstanceStatus(ctx, i.ID, alicloud.StatusStarting); err != nil {
		return err
	}
	return s.st.SetCDTInstancePlannedStop(ctx, i.ID, false)
}
func (b rotationBackend) Stop(ctx context.Context, i *store.CDTInstance) (bool, error) {
	s := b.s
	if !s.cdtLock(i.AccountID) {
		return false, fmt.Errorf("账号正在执行其他操作，稍后重试关机")
	}
	defer s.cdtUnlock(i.AccountID)
	client, err := s.cdtClient(ctx, i.AccountID)
	if err != nil {
		return false, err
	}
	status, err := client.DescribeInstanceStatus(ctx, i.InstanceID)
	if err != nil {
		return false, err
	}
	if i.Status == alicloud.StatusStopping && status == alicloud.StatusRunning && time.Since(i.UpdatedAt) < cdtStopStatusLagWindow {
		return false, nil
	}
	if status != alicloud.StatusStopped && status != alicloud.StatusStopping {
		if err := client.StopInstance(ctx, i.InstanceID, alicloud.StopModeCharging); err != nil {
			return false, err
		}
		status = alicloud.StatusStopping
	}
	if err := s.st.SetCDTInstanceStatus(ctx, i.ID, status); err != nil {
		return false, err
	}
	if err := s.st.SetCDTInstancePlannedStop(ctx, i.ID, true); err != nil {
		return false, err
	}
	s.cdtCtl.MarkNodePlannedStop(ctx, i.ID, "CDT 换班节省关机")
	return status == alicloud.StatusStopped, nil
}

func (s *Server) cdtRotationAccount(ctx context.Context, id int64) bool {
	c, err := s.st.LoadCDTRotation(ctx)
	// A corrupt configuration must not accidentally enable legacy auto-start.
	return err != nil || c.ManagesAccount(id)
}

func (s *Server) rotationRecordInUse(ctx context.Context, id int64) bool {
	c, err := s.st.LoadCDTRotation(ctx)
	return err != nil || (c.Enabled && c.DNSRecordID == id)
}

func (s *Server) handleGetCDTRotation(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.LoadCDTRotation(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	v, err := s.st.LoadCDTRotationState(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": c, "state": v})
}
func (s *Server) handleSaveCDTRotation(w http.ResponseWriter, r *http.Request) {
	var c store.CDTRotation
	if !decodeJSON(w, r, &c) {
		return
	}
	if c.Timezone == "" {
		c.Timezone = "Asia/Shanghai"
	}
	if err := s.rotation.Save(r.Context(), c); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.st.AddEvent(r.Context(), nil, store.EventCDTAction, store.LevelInfo, fmt.Sprintf("CDT 换班计划已保存，启用=%v", c.Enabled))
	s.handleGetCDTRotation(w, r)
}
