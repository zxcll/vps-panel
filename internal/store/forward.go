package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- 节点的转发配置 ---

const forwardNodeColumns = "node_id, relay_host, relay_host_v6, port_start, port_end, enabled, updated_at"

// 中间跳自动分配端口的默认范围。节点没配过转发时用它。
const (
	DefaultForwardPortStart = 20000
	DefaultForwardPortEnd   = 29999
)

// GetForwardNode 读节点的转发配置。没配过的节点返回一份默认值而不是错误 ——
// 「没配过」和「关掉了」是两回事，前者应该能直接用起来。
func (s *Store) GetForwardNode(ctx context.Context, nodeID int64) (*ForwardNode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+forwardNodeColumns+` FROM forward_nodes WHERE node_id = ?`, nodeID)
	fn, err := scanForwardNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultForwardNode(nodeID), nil
	}
	return fn, err
}

// AllForwardNodes 一次读出所有节点的转发配置，编排时防 N+1。
// 没配过的节点不会出现在结果里，调用方用 DefaultForwardNode 兜底。
func (s *Store) AllForwardNodes(ctx context.Context) (map[int64]*ForwardNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+forwardNodeColumns+` FROM forward_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]*ForwardNode{}
	for rows.Next() {
		fn, err := scanForwardNode(rows)
		if err != nil {
			return nil, err
		}
		out[fn.NodeID] = fn
	}
	return out, rows.Err()
}

// DefaultForwardNode 返回一份还没落库的默认配置。
func DefaultForwardNode(nodeID int64) *ForwardNode { return defaultForwardNode(nodeID) }

func defaultForwardNode(nodeID int64) *ForwardNode {
	return &ForwardNode{
		NodeID:    nodeID,
		PortStart: DefaultForwardPortStart,
		PortEnd:   DefaultForwardPortEnd,
		Enabled:   true,
	}
}

func (s *Store) SaveForwardNode(ctx context.Context, fn *ForwardNode) error {
	now := time.Now().UTC()
	fn.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO forward_nodes (`+forwardNodeColumns+`) VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
			relay_host    = excluded.relay_host,
			relay_host_v6 = excluded.relay_host_v6,
			port_start    = excluded.port_start,
			port_end      = excluded.port_end,
			enabled       = excluded.enabled,
			updated_at    = excluded.updated_at`,
		fn.NodeID, fn.RelayHost, fn.RelayHostV6, fn.PortStart, fn.PortEnd, boolInt(fn.Enabled), timeVal(now))
	return err
}

func scanForwardNode(sc interface{ Scan(...any) error }) (*ForwardNode, error) {
	var fn ForwardNode
	var enabled int
	var updated int64
	if err := sc.Scan(&fn.NodeID, &fn.RelayHost, &fn.RelayHostV6,
		&fn.PortStart, &fn.PortEnd, &enabled, &updated); err != nil {
		return nil, err
	}
	fn.Enabled = enabled != 0
	fn.UpdatedAt = fromUnix(updated)
	return &fn, nil
}

// --- 转发规则 ---

const forwardRuleColumns = "id, name, proto, dest_host, dest_port, enabled, remark, created_at, updated_at"

// ListForwardRules 列出所有规则，每条都带上按 position 升序的跳。
func (s *Store) ListForwardRules(ctx context.Context) ([]*ForwardRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+forwardRuleColumns+` FROM forward_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*ForwardRule{}
	byID := map[int64]*ForwardRule{}
	for rows.Next() {
		r, err := scanForwardRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 一次把所有跳捞出来再分发，别在循环里逐条查。
	hops, err := s.allForwardHops(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hops {
		if r, ok := byID[h.RuleID]; ok {
			r.Hops = append(r.Hops, h)
		}
	}
	return out, nil
}

func (s *Store) GetForwardRule(ctx context.Context, id int64) (*ForwardRule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+forwardRuleColumns+` FROM forward_rules WHERE id = ?`, id)
	r, err := scanForwardRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Hops, err = s.forwardHopsOf(ctx, id)
	return r, err
}

func scanForwardRule(sc interface{ Scan(...any) error }) (*ForwardRule, error) {
	var r ForwardRule
	var enabled int
	var created, updated int64
	if err := sc.Scan(&r.ID, &r.Name, &r.Proto, &r.DestHost, &r.DestPort,
		&enabled, &r.Remark, &created, &updated); err != nil {
		return nil, err
	}
	r.Enabled = enabled != 0
	r.CreatedAt, r.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &r, nil
}

// CreateForwardRule 建规则并写入它的跳，整体在一个事务里 ——
// 只有规则没有跳的半成品在编排层是无法处理的。
func (s *Store) CreateForwardRule(ctx context.Context, r *ForwardRule) error {
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	return s.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.tx.ExecContext(ctx,
			`INSERT INTO forward_rules (name, proto, dest_host, dest_port, enabled, remark, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			r.Name, r.Proto, r.DestHost, r.DestPort, boolInt(r.Enabled), r.Remark, timeVal(now), timeVal(now))
		if err != nil {
			return err
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return replaceHops(ctx, tx, r)
	})
}

// UpdateForwardRule 更新规则并整组重写它的跳。
//
// 跳是整组替换而不是逐条 diff：跳序、端口、模式往往一起变，
// diff 的中间态会短暂违反 (node_id, listen_port, proto) 唯一约束。
// 代价是跳的主键会变 —— 而 hop_id 正是转发账本的主键，所以
// 编辑规则等于把这条规则的转发用量清零。这是有意的取舍：
// 改了跳序之后，旧的用量本来也对不上新的拓扑了。
func (s *Store) UpdateForwardRule(ctx context.Context, r *ForwardRule) error {
	now := time.Now().UTC()
	r.UpdatedAt = now
	return s.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.tx.ExecContext(ctx,
			`UPDATE forward_rules SET name=?, proto=?, dest_host=?, dest_port=?, enabled=?, remark=?, updated_at=?
			 WHERE id = ?`,
			r.Name, r.Proto, r.DestHost, r.DestPort, boolInt(r.Enabled), r.Remark, timeVal(now), r.ID)
		if err != nil {
			return err
		}
		if err := affected(res); err != nil {
			return err
		}
		return replaceHops(ctx, tx, r)
	})
}

func (s *Store) DeleteForwardRule(ctx context.Context, id int64) error {
	// forward_hops 及其账本靠外键 ON DELETE CASCADE 一起清掉。
	res, err := s.db.ExecContext(ctx, `DELETE FROM forward_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetForwardRuleEnabled 只切开关，不动跳，因此不会清掉转发用量。
func (s *Store) SetForwardRuleEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE forward_rules SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(enabled), timeVal(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// replaceHops 删掉规则现有的跳再按 r.Hops 重建，并把生成的 ID 写回 r.Hops。
func replaceHops(ctx context.Context, tx *Tx, r *ForwardRule) error {
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM forward_hops WHERE rule_id = ?`, r.ID); err != nil {
		return err
	}
	for i := range r.Hops {
		h := &r.Hops[i]
		h.RuleID = r.ID
		h.Position = i
		// proto 从规则冗余下来，保证唯一索引按协议区分坑位。
		h.Proto = r.Proto
		res, err := tx.tx.ExecContext(ctx,
			`INSERT INTO forward_hops (rule_id, position, node_id, listen_port, proto, mode, bandwidth_mbps)
			 VALUES (?,?,?,?,?,?,?)`,
			h.RuleID, h.Position, h.NodeID, h.ListenPort, h.Proto, h.Mode, h.BandwidthMbps)
		if err != nil {
			return err
		}
		if h.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	return nil
}

// --- 跳 ---

const forwardHopColumns = "h.id, h.rule_id, h.position, h.node_id, h.listen_port, h.proto, h.mode, h.bandwidth_mbps"

func (s *Store) allForwardHops(ctx context.Context) ([]ForwardHop, error) {
	return s.queryHops(ctx,
		`SELECT `+forwardHopColumns+`, COALESCE(n.name, '')
		 FROM forward_hops h LEFT JOIN nodes n ON n.id = h.node_id
		 ORDER BY h.rule_id, h.position`)
}

func (s *Store) forwardHopsOf(ctx context.Context, ruleID int64) ([]ForwardHop, error) {
	return s.queryHops(ctx,
		`SELECT `+forwardHopColumns+`, COALESCE(n.name, '')
		 FROM forward_hops h LEFT JOIN nodes n ON n.id = h.node_id
		 WHERE h.rule_id = ? ORDER BY h.position`, ruleID)
}

// ForwardHopsOnNode 列出某个节点上的所有跳，用于下发该节点的完整规则集。
func (s *Store) ForwardHopsOnNode(ctx context.Context, nodeID int64) ([]ForwardHop, error) {
	return s.queryHops(ctx,
		`SELECT `+forwardHopColumns+`, COALESCE(n.name, '')
		 FROM forward_hops h LEFT JOIN nodes n ON n.id = h.node_id
		 WHERE h.node_id = ? ORDER BY h.rule_id, h.position`, nodeID)
}

func (s *Store) queryHops(ctx context.Context, q string, args ...any) ([]ForwardHop, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ForwardHop{}
	for rows.Next() {
		var h ForwardHop
		if err := rows.Scan(&h.ID, &h.RuleID, &h.Position, &h.NodeID,
			&h.ListenPort, &h.Proto, &h.Mode, &h.BandwidthMbps, &h.NodeName); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UsedForwardPorts 返回某节点上已被占用的 (端口, 协议)，供自动分配端口时避让。
// excludeRule 用于编辑规则时排除自己现有的跳（否则会和自己冲突）。
func (s *Store) UsedForwardPorts(ctx context.Context, nodeID, excludeRule int64) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT listen_port, proto FROM forward_hops WHERE node_id = ? AND rule_id != ?`,
		nodeID, excludeRule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]string{}
	for rows.Next() {
		var port int
		var proto string
		if err := rows.Scan(&port, &proto); err != nil {
			return nil, err
		}
		out[port] = proto
	}
	return out, rows.Err()
}

// --- 转发账本 ---
//
// 这一组和节点账本（GetCounter/AddUsage/AddHourly）是同构的，
// 但写的是另一套表，**绝不参与配额判定**。原因见 0002_forward.sql 里的注释。

// GetForwardCounter 读某跳上次的计数快照。
// epoch 为空说明还没真正上报过，等价于 GetCounter 里 boot_id 为空的判断。
func (t *Tx) GetForwardCounter(ctx context.Context, hopID int64) (*ForwardCounter, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT hop_id, epoch, last_up, last_down, updated_at FROM forward_counters WHERE hop_id = ?`, hopID)
	var c ForwardCounter
	var updated int64
	err := row.Scan(&c.HopID, &c.Epoch, &c.LastUp, &c.LastDown, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.Epoch == "" {
		return nil, nil
	}
	c.UpdatedAt = fromUnix(updated)
	return &c, nil
}

func (t *Tx) SaveForwardCounter(ctx context.Context, c *ForwardCounter) error {
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO forward_counters (hop_id, epoch, last_up, last_down, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(hop_id) DO UPDATE SET
			epoch = excluded.epoch, last_up = excluded.last_up,
			last_down = excluded.last_down, updated_at = excluded.updated_at`,
		c.HopID, c.Epoch, c.LastUp, c.LastDown, timeVal(c.UpdatedAt))
	return err
}

// AddForwardUsage 把增量累加进当前周期的用量。
func (t *Tx) AddForwardUsage(ctx context.Context, hopID, dup, ddown int64, cycleStart, at time.Time) error {
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO forward_usage (hop_id, cycle_start, up_bytes, down_bytes, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(hop_id) DO UPDATE SET
			up_bytes   = forward_usage.up_bytes   + excluded.up_bytes,
			down_bytes = forward_usage.down_bytes + excluded.down_bytes,
			updated_at = excluded.updated_at`,
		hopID, timeVal(cycleStart), dup, ddown, timeVal(at))
	return err
}

func (t *Tx) AddForwardHourly(ctx context.Context, hopID int64, hour time.Time, dup, ddown int64) error {
	bucket := hour.UTC().Truncate(time.Hour)
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO forward_hourly (hop_id, hour_ts, up_delta, down_delta) VALUES (?,?,?,?)
		 ON CONFLICT(hop_id, hour_ts) DO UPDATE SET
			up_delta   = forward_hourly.up_delta   + excluded.up_delta,
			down_delta = forward_hourly.down_delta + excluded.down_delta`,
		hopID, timeVal(bucket), dup, ddown)
	return err
}

// HopExists 判断这个 hop_id 还在不在。
//
// 探针会继续上报刚被删掉的跳（它要把最后一段流量送回来），面板这边
// 认不出来的 hop_id 必须直接忽略，否则外键约束会让整个上报事务回滚。
func (t *Tx) HopExists(ctx context.Context, hopID int64) (bool, error) {
	var one int
	err := t.tx.QueryRowContext(ctx, `SELECT 1 FROM forward_hops WHERE id = ?`, hopID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AllForwardUsage 一次读出所有跳的当前用量，列表页防 N+1。
func (s *Store) AllForwardUsage(ctx context.Context) (map[int64]*ForwardUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hop_id, cycle_start, up_bytes, down_bytes, updated_at FROM forward_usage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]*ForwardUsage{}
	for rows.Next() {
		var u ForwardUsage
		var cycle, updated int64
		if err := rows.Scan(&u.HopID, &cycle, &u.UpBytes, &u.DownBytes, &updated); err != nil {
			return nil, err
		}
		u.CycleStart, u.UpdatedAt = fromUnix(cycle), fromUnix(updated)
		out[u.HopID] = &u
	}
	return out, rows.Err()
}

// ResetForwardUsageOfNode 清掉某节点上所有跳的转发用量。
// 节点账单周期翻页时调用，让转发分账和节点账本的周期对齐。
func (s *Store) ResetForwardUsageOfNode(ctx context.Context, nodeID int64, cycleStart time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE forward_usage SET up_bytes = 0, down_bytes = 0, cycle_start = ?, updated_at = ?
		 WHERE hop_id IN (SELECT id FROM forward_hops WHERE node_id = ?)`,
		timeVal(cycleStart), timeVal(time.Now().UTC()), nodeID)
	return err
}

// ForwardHourlyRange 取某跳的小时曲线。
func (s *Store) ForwardHourlyRange(ctx context.Context, hopID int64, from, to time.Time) ([]ForwardHourlyPoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hour_ts, up_delta, down_delta FROM forward_hourly
		 WHERE hop_id = ? AND hour_ts >= ? AND hour_ts < ? ORDER BY hour_ts`,
		hopID, timeVal(from), timeVal(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ForwardHourlyPoint{}
	for rows.Next() {
		var p ForwardHourlyPoint
		var ts int64
		if err := rows.Scan(&ts, &p.UpDelta, &p.DownDelta); err != nil {
			return nil, err
		}
		p.HourTS = fromUnix(ts)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneForwardHourly 清掉过期的小时数据，和 PruneHourly 一起被引擎调用。
func (s *Store) PruneForwardHourly(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM forward_hourly WHERE hour_ts < ?`, timeVal(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
