package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// 事件类型。
const (
	EventNodeOnline    = "node_online"
	EventNodeOffline   = "node_offline"
	EventTrafficWarn   = "traffic_warn"
	EventTrafficExceed = "traffic_exceed"
	EventActionRun     = "action_run"
	EventDNSSwitch     = "dns_switch"
	EventCycleReset    = "cycle_reset"
	EventAgentReport   = "agent_report"
	EventForwardApply  = "forward_apply"
	// EventCDTSync 是阿里云 CDT 的同步结果（拉流量/账单/实例）。
	EventCDTSync = "cdt_sync"
	// EventCDTAction 是面板对阿里云实例真正动了手：熔断停机、保活拉起、
	// 定时开关机。这类事件一定要留痕，机器被停了得能查到是谁停的、为什么。
	EventCDTAction    = "cdt_action"
	EventSystem       = "system"
)

func (s *Store) AddEvent(ctx context.Context, nodeID *int64, typ, level, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (node_id, type, level, message, created_at) VALUES (?,?,?,?,?)`,
		nullInt64(nodeID), typ, level, msg, timeVal(time.Now().UTC()))
	return err
}

type EventFilter struct {
	NodeID *int64
	Level  string
	Type   string
	Limit  int
}

func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]*Event, error) {
	var where []string
	var args []any

	if f.NodeID != nil {
		where = append(where, "node_id = ?")
		args = append(args, *f.NodeID)
	}
	if f.Level != "" {
		where = append(where, "level = ?")
		args = append(args, f.Level)
	}
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}

	q := `SELECT id, node_id, type, level, message, created_at FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += " ORDER BY id DESC LIMIT " + strconv.Itoa(limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Event{}
	for rows.Next() {
		var e Event
		var nid sql.NullInt64
		var created int64
		if err := rows.Scan(&e.ID, &nid, &e.Type, &e.Level, &e.Message, &created); err != nil {
			return nil, err
		}
		e.NodeID = int64Ptr(nid)
		e.CreatedAt = fromUnix(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// PruneEvents 只保留最近 keep 条事件，防止日志表无限增长。
func (s *Store) PruneEvents(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = 20000
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM events WHERE id NOT IN (
			SELECT id FROM events ORDER BY id DESC LIMIT ?
		)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
