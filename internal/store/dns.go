package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- DNS 服务商凭据 ---

func (s *Store) ListDNSProviders(ctx context.Context) ([]*DNSProvider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, cred_enc, created_at, updated_at FROM dns_providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*DNSProvider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetDNSProvider(ctx context.Context, id int64) (*DNSProvider, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, type, cred_enc, created_at, updated_at FROM dns_providers WHERE id = ?`, id)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func scanProvider(sc interface{ Scan(...any) error }) (*DNSProvider, error) {
	var p DNSProvider
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.CredEnc, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &p, nil
}

func (s *Store) CreateDNSProvider(ctx context.Context, p *DNSProvider) error {
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO dns_providers (name, type, cred_enc, created_at, updated_at) VALUES (?,?,?,?,?)`,
		p.Name, p.Type, p.CredEnc, timeVal(now), timeVal(now))
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

// UpdateDNSProvider 更新服务商。credEnc 为 nil 表示不改凭据（用户没重填密钥）。
func (s *Store) UpdateDNSProvider(ctx context.Context, id int64, name, typ string, credEnc []byte) error {
	now := timeVal(time.Now().UTC())
	var res sql.Result
	var err error
	if credEnc == nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE dns_providers SET name=?, type=?, updated_at=? WHERE id=?`, name, typ, now, id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE dns_providers SET name=?, type=?, cred_enc=?, updated_at=? WHERE id=?`,
			name, typ, credEnc, now, id)
	}
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeleteDNSProvider(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dns_providers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// --- DNS 记录组 ---

const dnsRecordColumns = `id, provider_id, zone, name, record_type, ttl, proxied, strategy,
	switch_on_exceed, switch_on_offline, switch_on_warn,
	current_node_id, current_value, last_switch_at, last_error, enabled, created_at, updated_at`

func scanDNSRecord(sc interface{ Scan(...any) error }) (*DNSRecord, error) {
	var r DNSRecord
	var proxied, onExceed, onOffline, onWarn, enabled int
	var curNode, lastSwitch sql.NullInt64
	var created, updated int64

	if err := sc.Scan(&r.ID, &r.ProviderID, &r.Zone, &r.Name, &r.RecordType, &r.TTL, &proxied, &r.Strategy,
		&onExceed, &onOffline, &onWarn,
		&curNode, &r.CurrentValue, &lastSwitch, &r.LastError, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	r.Proxied = proxied != 0
	r.SwitchOnExceed = onExceed != 0
	r.SwitchOnOffline = onOffline != 0
	r.SwitchOnWarn = onWarn != 0
	r.Enabled = enabled != 0
	r.CurrentNodeID = int64Ptr(curNode)
	r.LastSwitchAt = timePtr(lastSwitch)
	r.CreatedAt, r.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &r, nil
}

// ListDNSRecords 返回所有记录组，并填充各自的候选节点（按优先级升序）。
func (s *Store) ListDNSRecords(ctx context.Context) ([]*DNSRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dnsRecordColumns+` FROM dns_records ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*DNSRecord{}
	for rows.Next() {
		r, err := scanDNSRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	members, err := s.allDNSMembers(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range out {
		r.Members = members[r.ID]
	}
	return out, nil
}

func (s *Store) GetDNSRecord(ctx context.Context, id int64) (*DNSRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+dnsRecordColumns+` FROM dns_records WHERE id = ?`, id)
	r, err := scanDNSRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Members, err = s.DNSMembers(ctx, id)
	return r, err
}

func (s *Store) CreateDNSRecord(ctx context.Context, r *DNSRecord) error {
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO dns_records (provider_id, zone, name, record_type, ttl, proxied, strategy,
			switch_on_exceed, switch_on_offline, switch_on_warn, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ProviderID, r.Zone, r.Name, r.RecordType, r.TTL, boolInt(r.Proxied), r.Strategy,
		boolInt(r.SwitchOnExceed), boolInt(r.SwitchOnOffline), boolInt(r.SwitchOnWarn),
		boolInt(r.Enabled), timeVal(now), timeVal(now))
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateDNSRecord(ctx context.Context, r *DNSRecord) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE dns_records SET provider_id=?, zone=?, name=?, record_type=?, ttl=?, proxied=?,
			strategy=?, switch_on_exceed=?, switch_on_offline=?, switch_on_warn=?, enabled=?, updated_at=?
		WHERE id=?`,
		r.ProviderID, r.Zone, r.Name, r.RecordType, r.TTL, boolInt(r.Proxied),
		r.Strategy, boolInt(r.SwitchOnExceed), boolInt(r.SwitchOnOffline), boolInt(r.SwitchOnWarn),
		boolInt(r.Enabled), timeVal(time.Now().UTC()), r.ID)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeleteDNSRecord(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dns_records WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetDNSCurrent 记录一次成功的切换结果。
func (s *Store) SetDNSCurrent(ctx context.Context, recordID, nodeID int64, value string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE dns_records SET current_node_id=?, current_value=?, last_switch_at=?, last_error='', updated_at=?
		WHERE id=?`,
		nodeID, value, timeVal(at), timeVal(time.Now().UTC()), recordID)
	return err
}

// SetDNSError 记录一次失败的切换，便于在面板上看到原因。
func (s *Store) SetDNSError(ctx context.Context, recordID int64, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dns_records SET last_error=?, updated_at=? WHERE id=?`,
		msg, timeVal(time.Now().UTC()), recordID)
	return err
}

// --- 候选节点 ---

func (s *Store) DNSMembers(ctx context.Context, recordID int64) ([]DNSMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.record_id, m.node_id, m.priority, COALESCE(n.name, '')
		FROM dns_record_members m LEFT JOIN nodes n ON n.id = m.node_id
		WHERE m.record_id = ? ORDER BY m.priority, m.node_id`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DNSMember{}
	for rows.Next() {
		var m DNSMember
		if err := rows.Scan(&m.RecordID, &m.NodeID, &m.Priority, &m.NodeName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) allDNSMembers(ctx context.Context) (map[int64][]DNSMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.record_id, m.node_id, m.priority, COALESCE(n.name, '')
		FROM dns_record_members m LEFT JOIN nodes n ON n.id = m.node_id
		ORDER BY m.record_id, m.priority, m.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64][]DNSMember)
	for rows.Next() {
		var m DNSMember
		if err := rows.Scan(&m.RecordID, &m.NodeID, &m.Priority, &m.NodeName); err != nil {
			return nil, err
		}
		out[m.RecordID] = append(out[m.RecordID], m)
	}
	return out, rows.Err()
}

// ReplaceDNSMembers 整组替换候选节点列表（前端是整体提交的）。
func (s *Store) ReplaceDNSMembers(ctx context.Context, recordID int64, members []DNSMember) error {
	return s.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx,
			`DELETE FROM dns_record_members WHERE record_id = ?`, recordID); err != nil {
			return err
		}
		for _, m := range members {
			if _, err := tx.tx.ExecContext(ctx,
				`INSERT INTO dns_record_members (record_id, node_id, priority) VALUES (?,?,?)`,
				recordID, m.NodeID, m.Priority); err != nil {
				return err
			}
		}
		return nil
	})
}

// DNSRecordsForNode 找出把某个节点列为候选的所有记录组。
// 节点超额或掉线时，用它定位需要重新选主的记录。
func (s *Store) DNSRecordsForNode(ctx context.Context, nodeID int64) ([]*DNSRecord, error) {
	all, err := s.ListDNSRecords(ctx)
	if err != nil {
		return nil, err
	}
	var out []*DNSRecord
	for _, r := range all {
		for _, m := range r.Members {
			if m.NodeID == nodeID {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}
