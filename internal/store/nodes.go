package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrNotFound = errors.New("记录不存在")

const nodeColumns = `id, name, remark, secret, ipv4, ipv6,
	quota_bytes, billing_mode, traffic_ratio, warn_percent,
	reset_day, reset_tz, cycle_start, cycle_end,
	action_on_exceed, exceed_command, auto_recover_on_reset,
	ssh_host, ssh_port, ssh_user, ssh_auth, ssh_secret_enc, ssh_key_pass_enc,
	ssh_host_key, ssh_use_sudo, probe_port, cdt_instance_id,
	status, last_seen, enabled, exceed_handled_at, warn_handled_at,
	created_at, updated_at`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var cycleStart, cycleEnd, createdAt, updatedAt int64
	var lastSeen, exceedAt, warnAt sql.NullInt64
	var autoRecover, useSudo, enabled int

	err := sc.Scan(
		&n.ID, &n.Name, &n.Remark, &n.Secret, &n.IPv4, &n.IPv6,
		&n.QuotaBytes, &n.BillingMode, &n.TrafficRatio, &n.WarnPercent,
		&n.ResetDay, &n.ResetTZ, &cycleStart, &cycleEnd,
		&n.ActionOnExceed, &n.ExceedCommand, &autoRecover,
		&n.SSHHost, &n.SSHPort, &n.SSHUser, &n.SSHAuth, &n.SSHSecretEnc, &n.SSHKeyPassEnc,
		&n.SSHHostKey, &useSudo, &n.ProbePort, &n.CDTInstanceID,
		&n.Status, &lastSeen, &enabled, &exceedAt, &warnAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	n.CycleStart = fromUnix(cycleStart)
	n.CycleEnd = fromUnix(cycleEnd)
	n.CreatedAt = fromUnix(createdAt)
	n.UpdatedAt = fromUnix(updatedAt)
	n.LastSeen = timePtr(lastSeen)
	n.ExceedHandledAt = timePtr(exceedAt)
	n.WarnHandledAt = timePtr(warnAt)
	n.AutoRecoverOnReset = autoRecover != 0
	n.SSHUseSudo = useSudo != 0
	n.Enabled = enabled != 0
	return &n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) GetNode(ctx context.Context, id int64) (*Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// GetNodeBySecret 供探针鉴权使用。
func (s *Store) GetNodeBySecret(ctx context.Context, secret string) (*Node, error) {
	if secret == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE secret = ?`, secret)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	now := time.Now().UTC()
	n.CreatedAt, n.UpdatedAt = now, now

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (
			name, remark, secret, ipv4, ipv6,
			quota_bytes, billing_mode, traffic_ratio, warn_percent,
			reset_day, reset_tz, cycle_start, cycle_end,
			action_on_exceed, exceed_command, auto_recover_on_reset,
			ssh_host, ssh_port, ssh_user, ssh_auth, ssh_secret_enc, ssh_key_pass_enc,
			ssh_host_key, ssh_use_sudo, probe_port, cdt_instance_id,
			status, enabled, created_at, updated_at
		) VALUES (?,?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?,?, ?,?,?,?,?,?, ?,?,?,?, ?,?,?,?)`,
		n.Name, n.Remark, n.Secret, n.IPv4, n.IPv6,
		n.QuotaBytes, n.BillingMode, n.TrafficRatio, n.WarnPercent,
		n.ResetDay, n.ResetTZ, timeVal(n.CycleStart), timeVal(n.CycleEnd),
		n.ActionOnExceed, n.ExceedCommand, boolInt(n.AutoRecoverOnReset),
		n.SSHHost, n.SSHPort, n.SSHUser, n.SSHAuth, n.SSHSecretEnc, n.SSHKeyPassEnc,
		n.SSHHostKey, boolInt(n.SSHUseSudo), n.ProbePort, n.CDTInstanceID,
		n.Status, boolInt(n.Enabled), timeVal(now), timeVal(now),
	)
	if err != nil {
		return err
	}
	n.ID, err = res.LastInsertId()
	if err != nil {
		return err
	}

	// 建立空的用量与计数器行，后续都走 UPDATE，避免上报路径上出现 upsert 竞态。
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO node_usage (node_id, cycle_start, updated_at) VALUES (?,?,?)`,
		n.ID, timeVal(n.CycleStart), timeVal(now)); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO node_counters (node_id) VALUES (?)`, n.ID)
	return err
}

// UpdateNode 更新节点的可编辑字段。状态类字段（status/last_seen/周期/去重时间戳）
// 由后台流程单独维护，不在这里覆盖。
func (s *Store) UpdateNode(ctx context.Context, n *Node) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET
			name=?, remark=?, ipv4=?, ipv6=?,
			quota_bytes=?, billing_mode=?, traffic_ratio=?, warn_percent=?,
			reset_day=?, reset_tz=?,
			action_on_exceed=?, exceed_command=?, auto_recover_on_reset=?,
			ssh_host=?, ssh_port=?, ssh_user=?, ssh_auth=?,
			ssh_secret_enc=?, ssh_key_pass_enc=?, ssh_host_key=?, ssh_use_sudo=?,
			probe_port=?, cdt_instance_id=?, enabled=?, updated_at=?
		WHERE id=?`,
		n.Name, n.Remark, n.IPv4, n.IPv6,
		n.QuotaBytes, n.BillingMode, n.TrafficRatio, n.WarnPercent,
		n.ResetDay, n.ResetTZ,
		n.ActionOnExceed, n.ExceedCommand, boolInt(n.AutoRecoverOnReset),
		n.SSHHost, n.SSHPort, n.SSHUser, n.SSHAuth,
		n.SSHSecretEnc, n.SSHKeyPassEnc, n.SSHHostKey, boolInt(n.SSHUseSudo),
		n.ProbePort, n.CDTInstanceID, boolInt(n.Enabled), timeVal(now), n.ID,
	)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) SetNodeStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET status=?, updated_at=? WHERE id=?`,
		status, timeVal(time.Now().UTC()), id)
	return err
}

// TouchNode 记录心跳时间并把状态标为在线。
// 已经超额/被关停的节点不覆盖状态 —— 探针在关机前的最后几次心跳不该把状态刷回 online。
func (s *Store) TouchNode(ctx context.Context, id int64, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET
			last_seen = ?,
			status = CASE WHEN status IN ('exceeded','stopped') THEN status ELSE 'online' END,
			updated_at = ?
		WHERE id = ?`,
		timeVal(seen), timeVal(time.Now().UTC()), id)
	return err
}

func (s *Store) SetNodeHostKey(ctx context.Context, id int64, hostKey string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET ssh_host_key=?, updated_at=? WHERE id=?`,
		hostKey, timeVal(time.Now().UTC()), id)
	return err
}

// MarkExceedHandled 记录超额动作已执行，避免重复关机。
func (s *Store) MarkExceedHandled(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET exceed_handled_at=?, updated_at=? WHERE id=?`,
		timeVal(at), timeVal(time.Now().UTC()), id)
	return err
}

func (s *Store) MarkWarnHandled(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET warn_handled_at=?, updated_at=? WHERE id=?`,
		timeVal(at), timeVal(time.Now().UTC()), id)
	return err
}

// SetNodeCycle 写入新的账单周期边界，并清掉超额/预警去重标记。
func (s *Store) SetNodeCycle(ctx context.Context, id int64, start, end time.Time, clearHandled bool) error {
	q := `UPDATE nodes SET cycle_start=?, cycle_end=?, updated_at=?`
	if clearHandled {
		q += `, exceed_handled_at=NULL, warn_handled_at=NULL`
	}
	q += ` WHERE id=?`
	_, err := s.db.ExecContext(ctx, q, timeVal(start), timeVal(end), timeVal(time.Now().UTC()), id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// placeholders 生成 "?,?,?" 形式的占位符列表。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
