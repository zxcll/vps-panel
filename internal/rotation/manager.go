package rotation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/cdt"
	"github.com/zxcll/vps-panel/internal/store"
)

type Backend interface {
	CheckTraffic(context.Context, *store.CDTAccount) error
	RefreshInstance(context.Context, *store.CDTInstance) (*store.CDTInstance, error)
	Start(context.Context, *store.CDTInstance) error
	// Stop always requests StopCharging and returns true only once Stopped is observed.
	Stop(context.Context, *store.CDTInstance) (bool, error)
	SyncDNS(context.Context, int64, string) error
}

type Manager struct {
	mu      sync.Mutex
	st      *store.Store
	backend Backend
	report  func(string, string, string)
	now     func() time.Time
}

func New(st *store.Store, backend Backend, report func(string, string, string)) *Manager {
	return &Manager{st: st, backend: backend, report: report, now: time.Now}
}

func (m *Manager) emit(level, title, body string) {
	if m.report != nil {
		m.report(level, title, body)
	}
}

func (m *Manager) Save(ctx context.Context, c store.CDTRotation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Enabled {
		if err := Validate(c); err != nil {
			return err
		}
		r, err := m.st.GetDNSRecord(ctx, c.DNSRecordID)
		if err != nil {
			return fmt.Errorf("DDNS 记录不存在：%w", err)
		}
		if !r.Enabled || r.Strategy != store.StrategyCDTRotation || r.RecordType != "A" {
			return fmt.Errorf("请选择已启用、策略为「CDT 换班」的 A 记录")
		}
		if r.TTL > int(GracePeriod.Seconds()) {
			return fmt.Errorf("DNS TTL 不能超过 1800 秒（旧实例保留 30 分钟）")
		}
		for _, slot := range c.Slots {
			a, err := m.st.GetCDTAccount(ctx, slot.AccountID)
			if err != nil {
				return fmt.Errorf("账号不存在：%w", err)
			}
			if !a.Enabled {
				return fmt.Errorf("账号「%s」尚未启用", a.Name)
			}
			i, err := m.st.GetCDTInstance(ctx, slot.InstanceID)
			if err != nil || i.AccountID != a.ID {
				return fmt.Errorf("窗口选择的实例不属于账号「%s」或已被释放", a.Name)
			}
			if !i.Guarded {
				return fmt.Errorf("请先将实例「%s」设为受守护", i.InstanceID)
			}
		}
	}
	old, err := m.st.LoadCDTRotation(ctx)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(old, c) {
		return nil
	}
	return m.st.SaveCDTRotation(ctx, c)
}

func (m *Manager) Tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.st.LoadCDTRotation(ctx)
	if err != nil || !c.Enabled {
		return err
	}
	v, err := m.st.LoadCDTRotationState(ctx)
	if err != nil {
		return err
	}
	err = m.reconcile(ctx, c, &v, m.now())
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != v.LastError && msg != "" {
		m.emit(store.LevelError, "CDT 换班异常", msg)
	}
	v.LastError = msg
	return errors.Join(err, m.st.SaveCDTRotationState(ctx, v))
}

// Quota is checked before any start or DNS selection. A current-cycle trip is
// latched even if delayed metering later reports a smaller number.
func (m *Manager) account(ctx context.Context, id int64, now time.Time, recheck bool) (*store.CDTAccount, error) {
	a, err := m.st.GetCDTAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.Enabled {
		return a, nil
	}
	cycle := cdt.CycleOf(now)
	if a.Tripped() && a.TrippedCycle == string(cycle) {
		return a, nil
	}
	start, _ := cycle.Start()
	if recheck || a.Tripped() || a.LastSyncAt.Before(start) || now.Sub(a.LastSyncAt) > 2*a.SyncInterval()+time.Minute || a.LastError != "" {
		if err := m.backend.CheckTraffic(ctx, a); err != nil {
			return nil, fmt.Errorf("账号「%s」流量核实失败：%w", a.Name, err)
		}
		a, err = m.st.GetCDTAccount(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	rows, err := m.st.CDTTrafficOf(ctx, id, string(cycle))
	if err != nil {
		return nil, err
	}
	regions := map[string]int64{}
	for _, row := range rows {
		regions[row.BusinessRegionID] += row.TrafficBytes
	}
	status := cdt.EvaluateAt(cdt.SumByBucket(regions), cdt.QuotaFromGB(a.QuotaMainlandGB, a.QuotaOverseasGB), a.ThresholdPercent, now)
	reason := status.Reason
	if reason == "" && a.OutstandingThreshold > 0 {
		bill, err := m.st.GetCDTBill(ctx, id, string(cycle))
		if err != nil || bill == nil {
			return nil, fmt.Errorf("账号「%s」尚无本账期账单，暂不自动开机", a.Name)
		}
		if bill.Outstanding >= a.OutstandingThreshold {
			reason = "待还金额达到熔断线"
		}
	}
	if reason != "" {
		if !a.Tripped() || a.TrippedCycle != string(cycle) {
			if err := m.st.MarkCDTTripped(ctx, id, now, string(cycle), reason); err != nil {
				return nil, err
			}
		}
		return m.st.GetCDTAccount(ctx, id)
	}
	if a.Tripped() && a.TrippedCycle != string(cycle) {
		if a.LastSyncAt.Before(start) {
			return nil, fmt.Errorf("账号「%s」尚未同步新账期流量", a.Name)
		}
		if err := m.st.ClearCDTTripped(ctx, id); err != nil {
			return nil, err
		}
		m.emit(store.LevelInfo, "CDT 换班额度恢复", fmt.Sprintf("账号「%s」新账期流量已核实，恢复参与窗口调度", a.Name))
		return m.st.GetCDTAccount(ctx, id)
	}
	return a, nil
}

func (m *Manager) reconcile(ctx context.Context, c store.CDTRotation, v *store.CDTRotationState, now time.Time) error {
	if err := Validate(c); err != nil {
		return err
	}
	accounts := map[int64]*store.CDTAccount{}
	instances := map[int64]*store.CDTInstance{}
	var failures []error
	// Exhaustion bypasses DNS and the grace period, including pending old instances.
	for _, slot := range c.Slots {
		a, err := m.account(ctx, slot.AccountID, now, false)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		accounts[a.ID] = a
		i, err := m.st.GetCDTInstance(ctx, slot.InstanceID)
		if err != nil || i.AccountID != a.ID {
			failures = append(failures, fmt.Errorf("账号「%s」的换班实例不存在，请重新选择", a.Name))
			continue
		}
		if !i.Guarded {
			failures = append(failures, fmt.Errorf("实例 %s 未受守护，已暂停其换班动作", i.InstanceID))
			continue
		}
		instances[i.ID] = i
		if a.Enabled && a.Tripped() {
			done, err := m.backend.Stop(ctx, i)
			if err != nil {
				failures = append(failures, fmt.Errorf("超限实例 %s 节省关机失败：%w", i.InstanceID, err))
			}
			if done {
				delete(v.PendingStops, i.ID)
			}
		}
	}
	var target *store.CDTInstance
	var account *store.CDTAccount
	for _, slot := range Candidates(c, now) {
		a, i := accounts[slot.AccountID], instances[slot.InstanceID]
		if a == nil || !a.Enabled || a.Tripped() || i == nil {
			continue
		}
		// Recheck against the cloud on handover, rather than starting a backup
		// using an arbitrarily old traffic snapshot.
		if i.ID != v.CurrentInstanceID || i.Status == alicloud.StatusStopped {
			var err error
			a, err = m.account(ctx, a.ID, now, true)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if a.Tripped() {
				_, err := m.backend.Stop(ctx, i)
				if err != nil {
					failures = append(failures, err)
				}
				continue
			}
		}
		target, account = i, a
		break
	}
	if target == nil {
		return errors.Join(append(failures, fmt.Errorf("没有可参与 DDNS 的账号：额度耗尽的实例保持节省停机，等待新账期；当前解析保持不变"))...)
	}
	// A previously retiring instance may become the active fallback again.
	delete(v.PendingStops, target.ID)
	fresh, err := m.backend.RefreshInstance(ctx, target)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	if fresh.Status == alicloud.StatusStopped {
		if err := m.backend.Start(ctx, fresh); err != nil {
			return errors.Join(append(failures, err)...)
		}
		return errors.Join(failures...)
	}
	if fresh.Status != alicloud.StatusRunning {
		return errors.Join(failures...)
	}
	ip := net.ParseIP(fresh.PublicIP)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() {
		return fmt.Errorf("当班实例 %s 尚未取得公网 IPv4，保留旧实例", fresh.InstanceID)
	}
	// An asynchronous traffic sync may have tripped this account while ECS was queried.
	latest, err := m.st.GetCDTAccount(ctx, target.AccountID)
	if err != nil {
		return err
	}
	if latest.Tripped() || !latest.Enabled {
		return fmt.Errorf("当班账号已熔断或停用，取消本次 DDNS 更新")
	}
	if err := m.backend.SyncDNS(ctx, c.DNSRecordID, fresh.PublicIP); err != nil {
		return errors.Join(append(failures, fmt.Errorf("DDNS 更新失败，保留旧实例：%w", err))...)
	}
	changed := v.CurrentInstanceID != target.ID || v.CurrentIP != fresh.PublicIP
	if changed {
		v.CurrentInstanceID, v.CurrentIP = target.ID, fresh.PublicIP
		v.LastSwitchAt = m.now()
	}
	// Enrol all non-active instances after the first successful DNS write, so
	// enabling a plan while several machines are running also converges safely.
	for _, slot := range c.Slots {
		a, i := accounts[slot.AccountID], instances[slot.InstanceID]
		if i == nil || a == nil || !a.Enabled || a.Tripped() || i.ID == target.ID {
			continue
		}
		if _, ok := v.PendingStops[i.ID]; !ok && i.Status != alicloud.StatusStopped {
			v.PendingStops[i.ID] = m.now().Add(GracePeriod)
		}
	}
	// Never power off using a deadline that has not been durably recorded.
	if err := m.st.SaveCDTRotationState(ctx, *v); err != nil {
		return err
	}
	if changed {
		m.emit(store.LevelInfo, "CDT 换班解析已更新", fmt.Sprintf("当班账号「%s」，公网 IP：%s。DDNS 已核实，旧实例保留 30 分钟后节省关机。", account.Name, fresh.PublicIP))
	}
	for _, slot := range c.Slots {
		id := slot.InstanceID
		deadline, pending := v.PendingStops[id]
		if !pending {
			continue
		}
		if now.Before(deadline) || id == target.ID {
			continue
		}
		i := instances[id]
		if i == nil {
			continue
		}
		a := accounts[i.AccountID]
		if a == nil || !a.Enabled {
			continue
		}
		done, err := m.backend.Stop(ctx, i)
		if err != nil {
			failures = append(failures, fmt.Errorf("旧实例 %s 节省关机失败，将重试：%w", i.InstanceID, err))
			continue
		}
		if done {
			delete(v.PendingStops, id)
			m.emit(store.LevelInfo, "CDT 换班旧实例已停止", fmt.Sprintf("实例 %s 的 30 分钟等待已结束，已确认停止；停机使用节省模式。", i.InstanceID))
		}
	}
	return errors.Join(failures...)
}
