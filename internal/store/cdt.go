package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- 账号 ---

const cdtAccountColumns = `id, name, access_key_id, region_id, site_type,
	quota_mainland_gb, quota_overseas_gb, threshold_percent, outstanding_threshold,
	shutdown_mode, keep_alive, auto_start_time, auto_stop_time, schedule_tz,
	sync_interval_sec, tripped_at, tripped_reason, tripped_cycle, nostock_notified,
	last_sync_at, last_error, enabled, created_at, updated_at`

func (s *Store) ListCDTAccounts(ctx context.Context) ([]*CDTAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cdtAccountColumns+` FROM cdt_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*CDTAccount{}
	for rows.Next() {
		a, err := scanCDTAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetCDTAccount(ctx context.Context, id int64) (*CDTAccount, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cdtAccountColumns+` FROM cdt_accounts WHERE id = ?`, id)
	a, err := scanCDTAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// CDTAccountCred 单独读凭据密文。
//
// 和 DNS 凭据一样，密文不进 CDTAccount 结构体 —— 那个结构体是要直接
// JSON 序列化给前端的，把密文放进去迟早会有人忘了加 json:"-" 而漏出去。
func (s *Store) CDTAccountCred(ctx context.Context, id int64) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT cred_enc FROM cdt_accounts WHERE id = ?`, id).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return enc, err
}

func (s *Store) CreateCDTAccount(ctx context.Context, a *CDTAccount, credEnc []byte) error {
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO cdt_accounts (name, access_key_id, cred_enc, region_id, site_type,
			quota_mainland_gb, quota_overseas_gb, threshold_percent, outstanding_threshold,
			shutdown_mode, keep_alive, auto_start_time, auto_stop_time, schedule_tz,
			sync_interval_sec, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Name, a.AccessKeyID, credEnc, a.RegionID, a.SiteType,
		a.QuotaMainlandGB, a.QuotaOverseasGB, a.ThresholdPercent, a.OutstandingThreshold,
		a.ShutdownMode, boolInt(a.KeepAlive), a.AutoStartTime, a.AutoStopTime, a.ScheduleTZ,
		a.SyncIntervalSec, boolInt(a.Enabled), timeVal(now), timeVal(now))
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// UpdateCDTAccount 更新账号配置。credEnc 为 nil 表示不改凭据 ——
// 前端编辑时不会回填 Secret（它从不出网），留空就该保持原样。
func (s *Store) UpdateCDTAccount(ctx context.Context, a *CDTAccount, credEnc []byte) error {
	now := time.Now().UTC()
	a.UpdatedAt = now

	query := `UPDATE cdt_accounts SET name=?, access_key_id=?, region_id=?, site_type=?,
			quota_mainland_gb=?, quota_overseas_gb=?, threshold_percent=?, outstanding_threshold=?,
			shutdown_mode=?, keep_alive=?, auto_start_time=?, auto_stop_time=?, schedule_tz=?,
			sync_interval_sec=?, enabled=?, updated_at=?`
	args := []any{
		a.Name, a.AccessKeyID, a.RegionID, a.SiteType,
		a.QuotaMainlandGB, a.QuotaOverseasGB, a.ThresholdPercent, a.OutstandingThreshold,
		a.ShutdownMode, boolInt(a.KeepAlive), a.AutoStartTime, a.AutoStopTime, a.ScheduleTZ,
		a.SyncIntervalSec, boolInt(a.Enabled), timeVal(now),
	}
	if credEnc != nil {
		query += `, cred_enc=?`
		args = append(args, credEnc)
	}
	query += ` WHERE id = ?`
	args = append(args, a.ID)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeleteCDTAccount(ctx context.Context, id int64) error {
	// 实例、流量、账单靠外键 ON DELETE CASCADE 一起清掉。
	res, err := s.db.ExecContext(ctx, `DELETE FROM cdt_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// MarkCDTTripped 记下这个账号已被熔断停机。
//
// 先落标记再执行停机，和 nodes.exceed_handled_at 是同一个套路：
// 停机不可逆，宁可漏执行也不能因为进程重启而重复执行。
func (s *Store) MarkCDTTripped(ctx context.Context, id int64, at time.Time, cycle, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cdt_accounts SET tripped_at=?, tripped_cycle=?, tripped_reason=?, updated_at=?
		 WHERE id = ?`,
		timeVal(at), cycle, reason, timeVal(time.Now().UTC()), id)
	return err
}

// ClearCDTTripped 解除熔断标记。账期翻页或用户手动开机时调用。
func (s *Store) ClearCDTTripped(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cdt_accounts SET tripped_at=0, tripped_cycle='', tripped_reason='',
			nostock_notified=0, updated_at=? WHERE id = ?`,
		timeVal(time.Now().UTC()), id)
	return err
}

func (s *Store) SetCDTNoStockNotified(ctx context.Context, id int64, notified bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cdt_accounts SET nostock_notified=?, updated_at=? WHERE id = ?`,
		boolInt(notified), timeVal(time.Now().UTC()), id)
	return err
}

// MarkCDTSynced 记一次同步结果。errMsg 为空表示这次成功了。
func (s *Store) MarkCDTSynced(ctx context.Context, id int64, at time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cdt_accounts SET last_sync_at=?, last_error=?, updated_at=? WHERE id = ?`,
		timeVal(at), errMsg, timeVal(time.Now().UTC()), id)
	return err
}

func scanCDTAccount(sc interface{ Scan(...any) error }) (*CDTAccount, error) {
	var a CDTAccount
	var keepAlive, nostock, enabled int
	var trippedAt, lastSync, created, updated int64
	if err := sc.Scan(&a.ID, &a.Name, &a.AccessKeyID, &a.RegionID, &a.SiteType,
		&a.QuotaMainlandGB, &a.QuotaOverseasGB, &a.ThresholdPercent, &a.OutstandingThreshold,
		&a.ShutdownMode, &keepAlive, &a.AutoStartTime, &a.AutoStopTime, &a.ScheduleTZ,
		&a.SyncIntervalSec, &trippedAt, &a.TrippedReason, &a.TrippedCycle, &nostock,
		&lastSync, &a.LastError, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	a.KeepAlive = keepAlive != 0
	a.NoStockNotified = nostock != 0
	a.Enabled = enabled != 0
	a.TrippedAt = fromUnix(trippedAt)
	a.LastSyncAt = fromUnix(lastSync)
	a.CreatedAt, a.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &a, nil
}

// --- 实例 ---

const cdtInstanceColumns = `id, account_id, instance_id, instance_name, region_id, zone_id,
	status, public_ip, instance_type, bandwidth_mbps, is_spot, guarded, last_synced, updated_at`

func (s *Store) ListCDTInstances(ctx context.Context) ([]*CDTInstance, error) {
	return s.queryCDTInstances(ctx,
		`SELECT `+cdtInstanceColumns+` FROM cdt_instances ORDER BY account_id, instance_id`)
}

func (s *Store) CDTInstancesOf(ctx context.Context, accountID int64) ([]*CDTInstance, error) {
	return s.queryCDTInstances(ctx,
		`SELECT `+cdtInstanceColumns+` FROM cdt_instances WHERE account_id = ? ORDER BY instance_id`,
		accountID)
}

func (s *Store) GetCDTInstance(ctx context.Context, id int64) (*CDTInstance, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cdtInstanceColumns+` FROM cdt_instances WHERE id = ?`, id)
	inst, err := scanCDTInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return inst, err
}

// UpsertCDTInstance 写入或更新一台实例的快照。
//
// 注意 guarded 不在 UPDATE 列表里：它是用户设的，同步不能把它冲掉。
func (s *Store) UpsertCDTInstance(ctx context.Context, inst *CDTInstance) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cdt_instances (account_id, instance_id, instance_name, region_id, zone_id,
			status, public_ip, instance_type, bandwidth_mbps, is_spot, guarded, last_synced, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(account_id, instance_id) DO UPDATE SET
			instance_name  = excluded.instance_name,
			region_id      = excluded.region_id,
			zone_id        = excluded.zone_id,
			status         = excluded.status,
			public_ip      = excluded.public_ip,
			instance_type  = excluded.instance_type,
			bandwidth_mbps = excluded.bandwidth_mbps,
			is_spot        = excluded.is_spot,
			last_synced    = excluded.last_synced,
			updated_at     = excluded.updated_at`,
		inst.AccountID, inst.InstanceID, inst.InstanceName, inst.RegionID, inst.ZoneID,
		inst.Status, inst.PublicIP, inst.InstanceType, inst.BandwidthMbps,
		boolInt(inst.IsSpot), boolInt(inst.Guarded), timeVal(now), timeVal(now))
	return err
}

// SetCDTInstanceStatus 只更新状态，用于开关机之后立刻反映到界面上，
// 不用等下一轮同步。
func (s *Store) SetCDTInstanceStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cdt_instances SET status=?, updated_at=? WHERE id = ?`,
		status, timeVal(time.Now().UTC()), id)
	return err
}

func (s *Store) SetCDTInstanceGuarded(ctx context.Context, id int64, guarded bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE cdt_instances SET guarded=?, updated_at=? WHERE id = ?`,
		boolInt(guarded), timeVal(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// PruneCDTInstances 删掉某账号下已经不存在的实例。
// keep 是本次同步实际拉到的实例 ID 集合。
func (s *Store) PruneCDTInstances(ctx context.Context, accountID int64, keep map[string]bool) error {
	existing, err := s.CDTInstancesOf(ctx, accountID)
	if err != nil {
		return err
	}
	for _, inst := range existing {
		if keep[inst.InstanceID] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM cdt_instances WHERE id = ?`, inst.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) queryCDTInstances(ctx context.Context, q string, args ...any) ([]*CDTInstance, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*CDTInstance{}
	for rows.Next() {
		inst, err := scanCDTInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

func scanCDTInstance(sc interface{ Scan(...any) error }) (*CDTInstance, error) {
	var inst CDTInstance
	var isSpot, guarded int
	var synced, updated int64
	if err := sc.Scan(&inst.ID, &inst.AccountID, &inst.InstanceID, &inst.InstanceName,
		&inst.RegionID, &inst.ZoneID, &inst.Status, &inst.PublicIP, &inst.InstanceType,
		&inst.BandwidthMbps, &isSpot, &guarded, &synced, &updated); err != nil {
		return nil, err
	}
	inst.IsSpot = isSpot != 0
	inst.Guarded = guarded != 0
	inst.LastSynced = fromUnix(synced)
	inst.UpdatedAt = fromUnix(updated)
	return &inst, nil
}

// --- 流量快照 ---

// ReplaceCDTTraffic 用本次拉到的明细整体替换某账号在这个账期的流量快照。
//
// 整体替换而不是逐条 upsert：阿里云返回的是「本月至今的累计值」，
// 某个地域这个月没流量了就不会出现在结果里。只做 upsert 的话，
// 那条旧记录会一直挂在那儿，把用量算高，进而导致误熔断。
func (s *Store) ReplaceCDTTraffic(ctx context.Context, accountID int64, cycle string, details []CDTTraffic) error {
	now := time.Now().UTC()
	return s.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx,
			`DELETE FROM cdt_traffic WHERE account_id = ? AND cycle = ?`, accountID, cycle); err != nil {
			return err
		}
		for _, d := range details {
			if _, err := tx.tx.ExecContext(ctx,
				`INSERT INTO cdt_traffic (account_id, cycle, business_region_id,
					traffic_type, traffic_bytes, updated_at)
				 VALUES (?,?,?,?,?,?)
				 ON CONFLICT(account_id, cycle, business_region_id) DO UPDATE SET
					traffic_type  = excluded.traffic_type,
					traffic_bytes = excluded.traffic_bytes,
					updated_at    = excluded.updated_at`,
				accountID, cycle, d.BusinessRegionID, d.TrafficType, d.TrafficBytes, timeVal(now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// CDTTrafficOf 读某账号某账期的逐地域流量。
func (s *Store) CDTTrafficOf(ctx context.Context, accountID int64, cycle string) ([]CDTTraffic, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, cycle, business_region_id, traffic_type, traffic_bytes, updated_at
		 FROM cdt_traffic WHERE account_id = ? AND cycle = ? ORDER BY business_region_id`,
		accountID, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CDTTraffic{}
	for rows.Next() {
		var d CDTTraffic
		var updated int64
		if err := rows.Scan(&d.AccountID, &d.Cycle, &d.BusinessRegionID,
			&d.TrafficType, &d.TrafficBytes, &updated); err != nil {
			return nil, err
		}
		d.UpdatedAt = fromUnix(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- 余额与账单 ---

func (s *Store) SaveCDTBill(ctx context.Context, b *CDTBill) error {
	now := time.Now().UTC()
	b.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cdt_bill (account_id, cycle, available_amount, outstanding, currency, symbol, updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(account_id, cycle) DO UPDATE SET
			available_amount = excluded.available_amount,
			outstanding      = excluded.outstanding,
			currency         = excluded.currency,
			symbol           = excluded.symbol,
			updated_at       = excluded.updated_at`,
		b.AccountID, b.Cycle, b.AvailableAmount, b.Outstanding, b.Currency, b.Symbol, timeVal(now))
	return err
}

// GetCDTBill 读某账号某账期的账单快照。没有记录时返回 nil, nil ——
// 「还没同步过」不是错误，界面上显示成「—」即可。
func (s *Store) GetCDTBill(ctx context.Context, accountID int64, cycle string) (*CDTBill, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT account_id, cycle, available_amount, outstanding, currency, symbol, updated_at
		 FROM cdt_bill WHERE account_id = ? AND cycle = ?`, accountID, cycle)

	var b CDTBill
	var updated int64
	err := row.Scan(&b.AccountID, &b.Cycle, &b.AvailableAmount, &b.Outstanding,
		&b.Currency, &b.Symbol, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.UpdatedAt = fromUnix(updated)
	return &b, nil
}
