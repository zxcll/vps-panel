package store

import (
	"context"
	"encoding/json"
	"time"
)

const StrategyCDTRotation = "cdt_rotation"
const rotationConfigKey = "cdt_rotation_config"
const rotationStateKey = "cdt_rotation_state"

// CDTRotation owns one DNS record and one guarded instance per account.
type CDTRotation struct {
	Enabled     bool              `json:"enabled"`
	DNSRecordID int64             `json:"dns_record_id"`
	Timezone    string            `json:"timezone"`
	Slots       []CDTRotationSlot `json:"slots"`
}

type CDTRotationSlot struct {
	AccountID  int64  `json:"account_id"`
	InstanceID int64  `json:"instance_id"`
	Start      string `json:"start"`
	End        string `json:"end"`
}

// Deadlines survive process restarts. They are only created after DNS succeeds.
type CDTRotationState struct {
	CurrentInstanceID int64               `json:"current_instance_id"`
	CurrentIP         string              `json:"current_ip"`
	LastSwitchAt      time.Time           `json:"last_switch_at"`
	PendingStops      map[int64]time.Time `json:"pending_stops"`
	LastError         string              `json:"last_error"`
}

func (s *Store) LoadCDTRotation(ctx context.Context) (CDTRotation, error) {
	c := CDTRotation{Timezone: "Asia/Shanghai", Slots: []CDTRotationSlot{}}
	raw, err := s.GetSetting(ctx, rotationConfigKey)
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &c)
	}
	if c.Slots == nil {
		c.Slots = []CDTRotationSlot{}
	}
	return c, err
}

func (s *Store) LoadCDTRotationState(ctx context.Context) (CDTRotationState, error) {
	v := CDTRotationState{}
	raw, err := s.GetSetting(ctx, rotationStateKey)
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &v)
	}
	if v.PendingStops == nil {
		v.PendingStops = map[int64]time.Time{}
	}
	return v, err
}

// Saving a changed plan cancels its old deadlines; the new plan grants a fresh
// grace period after its first successful DNS reconciliation.
func (s *Store) SaveCDTRotation(ctx context.Context, c CDTRotation) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, rotationConfigKey, string(b)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, rotationStateKey); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveCDTRotationState(ctx context.Context, v CDTRotationState) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, rotationStateKey, string(b))
}

func (c CDTRotation) ManagesAccount(id int64) bool {
	if !c.Enabled {
		return false
	}
	for _, slot := range c.Slots {
		if slot.AccountID == id {
			return true
		}
	}
	return false
}

func (s *Store) SetDNSAddress(ctx context.Context, id int64, ip string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE dns_records SET current_node_id=NULL, current_value=?, last_switch_at=?, last_error='' WHERE id=?`, ip, timeVal(time.Now()), id)
	return err
}
