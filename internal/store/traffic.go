package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Tx 是一个事务句柄。流量入账必须在一个事务里完成
// （读计数器 → 算增量 → 写计数器 + 累计 + 小时聚合），否则并发上报会重复计账。
type Tx struct {
	tx *sql.Tx
}

// WithTx 在事务中执行 fn，fn 返回错误则回滚。
func (s *Store) WithTx(ctx context.Context, fn func(context.Context, *Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后回滚是 no-op

	if err := fn(ctx, &Tx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// --- 计数器 ---

// GetCounter 取上次上报的计数器快照。节点从未上报过时返回 nil。
func (t *Tx) GetCounter(ctx context.Context, nodeID int64) (*Counter, error) {
	var c Counter
	var updated int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT node_id, boot_id, iface, last_rx, last_tx, updated_at
		 FROM node_counters WHERE node_id = ?`, nodeID).
		Scan(&c.NodeID, &c.BootID, &c.Iface, &c.LastRx, &c.LastTx, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// boot_id 为空说明这行是建节点时插的占位，还没有真实上报过。
	if c.BootID == "" {
		return nil, nil
	}
	c.UpdatedAt = fromUnix(updated)
	return &c, nil
}

func (t *Tx) SaveCounter(ctx context.Context, c *Counter) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO node_counters (node_id, boot_id, iface, last_rx, last_tx, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
			boot_id=excluded.boot_id, iface=excluded.iface,
			last_rx=excluded.last_rx, last_tx=excluded.last_tx,
			updated_at=excluded.updated_at`,
		c.NodeID, c.BootID, c.Iface, c.LastRx, c.LastTx, timeVal(c.UpdatedAt))
	return err
}

// --- 周期累计 ---

// AddUsage 把增量累加到当前周期用量上，返回累加后的结果。
func (t *Tx) AddUsage(ctx context.Context, nodeID, drx, dtx int64, cycleStart, at time.Time) (*Usage, error) {
	if _, err := t.tx.ExecContext(ctx, `
		INSERT INTO node_usage (node_id, cycle_start, rx_bytes, tx_bytes, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
			rx_bytes = node_usage.rx_bytes + excluded.rx_bytes,
			tx_bytes = node_usage.tx_bytes + excluded.tx_bytes,
			updated_at = excluded.updated_at`,
		nodeID, timeVal(cycleStart), drx, dtx, timeVal(at)); err != nil {
		return nil, err
	}

	var u Usage
	var cs, up int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT node_id, cycle_start, rx_bytes, tx_bytes, updated_at FROM node_usage WHERE node_id = ?`,
		nodeID).Scan(&u.NodeID, &cs, &u.RxBytes, &u.TxBytes, &up)
	if err != nil {
		return nil, err
	}
	u.CycleStart = fromUnix(cs)
	u.UpdatedAt = fromUnix(up)
	return &u, nil
}

// AddHourly 累加到小时桶，用于画曲线。
func (t *Tx) AddHourly(ctx context.Context, nodeID int64, hour time.Time, drx, dtx int64) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO traffic_hourly (node_id, hour_ts, rx_delta, tx_delta)
		VALUES (?,?,?,?)
		ON CONFLICT(node_id, hour_ts) DO UPDATE SET
			rx_delta = traffic_hourly.rx_delta + excluded.rx_delta,
			tx_delta = traffic_hourly.tx_delta + excluded.tx_delta`,
		nodeID, timeVal(hour.UTC().Truncate(time.Hour)), drx, dtx)
	return err
}

// TouchNode 是 Store.TouchNode 的事务内版本。
func (t *Tx) TouchNode(ctx context.Context, id int64, seen time.Time) error {
	_, err := t.tx.ExecContext(ctx, `
		UPDATE nodes SET
			last_seen = ?,
			status = CASE WHEN status IN ('exceeded','stopped') THEN status ELSE 'online' END,
			updated_at = ?
		WHERE id = ?`,
		timeVal(seen), timeVal(time.Now().UTC()), id)
	return err
}

// --- 只读查询 ---

func (s *Store) GetUsage(ctx context.Context, nodeID int64) (*Usage, error) {
	var u Usage
	var cs, up int64
	err := s.db.QueryRowContext(ctx,
		`SELECT node_id, cycle_start, rx_bytes, tx_bytes, updated_at FROM node_usage WHERE node_id = ?`,
		nodeID).Scan(&u.NodeID, &cs, &u.RxBytes, &u.TxBytes, &up)
	if errors.Is(err, sql.ErrNoRows) {
		return &Usage{NodeID: nodeID}, nil
	}
	if err != nil {
		return nil, err
	}
	u.CycleStart = fromUnix(cs)
	u.UpdatedAt = fromUnix(up)
	return &u, nil
}

// AllUsage 一次取回所有节点的用量，避免总览页 N+1 查询。
func (s *Store) AllUsage(ctx context.Context) (map[int64]*Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, cycle_start, rx_bytes, tx_bytes, updated_at FROM node_usage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]*Usage)
	for rows.Next() {
		var u Usage
		var cs, up int64
		if err := rows.Scan(&u.NodeID, &cs, &u.RxBytes, &u.TxBytes, &up); err != nil {
			return nil, err
		}
		u.CycleStart = fromUnix(cs)
		u.UpdatedAt = fromUnix(up)
		out[u.NodeID] = &u
	}
	return out, rows.Err()
}

// ResetUsage 把周期用量清零并写入新的周期起点。
func (s *Store) ResetUsage(ctx context.Context, nodeID int64, cycleStart time.Time) error {
	now := timeVal(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_usage (node_id, cycle_start, rx_bytes, tx_bytes, updated_at)
		VALUES (?,?,0,0,?)
		ON CONFLICT(node_id) DO UPDATE SET
			cycle_start=excluded.cycle_start, rx_bytes=0, tx_bytes=0, updated_at=excluded.updated_at`,
		nodeID, timeVal(cycleStart), now)
	return err
}

// HourlyRange 取指定时间段的小时曲线数据。
func (s *Store) HourlyRange(ctx context.Context, nodeID int64, from, to time.Time) ([]HourlyPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hour_ts, rx_delta, tx_delta FROM traffic_hourly
		WHERE node_id = ? AND hour_ts >= ? AND hour_ts <= ?
		ORDER BY hour_ts`,
		nodeID, timeVal(from), timeVal(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HourlyPoint{}
	for rows.Next() {
		var p HourlyPoint
		var ts int64
		if err := rows.Scan(&ts, &p.RxDelta, &p.TxDelta); err != nil {
			return nil, err
		}
		p.HourTS = fromUnix(ts)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneHourly 删除过期的小时数据，防止库无限膨胀。
func (s *Store) PruneHourly(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM traffic_hourly WHERE hour_ts < ?`, timeVal(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- 周期归档 ---

func (s *Store) ArchiveCycle(ctx context.Context, rec *CycleRecord) error {
	rec.ArchivedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO cycle_history
			(node_id, cycle_start, cycle_end, rx_bytes, tx_bytes, billed_bytes, quota_bytes, billing_mode, archived_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rec.NodeID, timeVal(rec.CycleStart), timeVal(rec.CycleEnd),
		rec.RxBytes, rec.TxBytes, rec.BilledBytes, rec.QuotaBytes, rec.BillingMode, timeVal(rec.ArchivedAt))
	if err != nil {
		return err
	}
	rec.ID, err = res.LastInsertId()
	return err
}

func (s *Store) ListCycles(ctx context.Context, nodeID int64, limit int) ([]*CycleRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 24
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_id, cycle_start, cycle_end, rx_bytes, tx_bytes,
		       billed_bytes, quota_bytes, billing_mode, archived_at
		FROM cycle_history WHERE node_id = ? ORDER BY cycle_start DESC LIMIT ?`,
		nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*CycleRecord{}
	for rows.Next() {
		var r CycleRecord
		var cs, ce, at int64
		if err := rows.Scan(&r.ID, &r.NodeID, &cs, &ce, &r.RxBytes, &r.TxBytes,
			&r.BilledBytes, &r.QuotaBytes, &r.BillingMode, &at); err != nil {
			return nil, err
		}
		r.CycleStart, r.CycleEnd, r.ArchivedAt = fromUnix(cs), fromUnix(ce), fromUnix(at)
		out = append(out, &r)
	}
	return out, rows.Err()
}
