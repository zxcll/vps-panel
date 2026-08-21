package failover

import (
	"context"
	"fmt"
	"time"

	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/store"
)

// provider 构造（并缓存）某个服务商的客户端。
// 缓存的意义在于 Cloudflare 客户端会记住 zone ID，省掉每次切换多一次 API 调用。
func (m *Manager) provider(ctx context.Context, providerID int64) (dns.Provider, error) {
	rec, err := m.st.GetDNSProvider(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("读取 DNS 服务商配置: %w", err)
	}

	m.mu.Lock()
	if c, ok := m.providers[providerID]; ok && c.updated.Equal(rec.UpdatedAt) {
		m.mu.Unlock()
		return c.p, nil
	}
	m.mu.Unlock()

	raw, err := m.cipher.Decrypt(rec.CredEnc)
	if err != nil {
		return nil, fmt.Errorf("解密 %s 的凭据: %w", rec.Name, err)
	}
	cred, err := dns.ParseCredentials(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 的凭据: %w", rec.Name, err)
	}
	p, err := dns.New(rec.Type, cred)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.providers[providerID] = &cachedProvider{p: p, updated: rec.UpdatedAt}
	m.mu.Unlock()
	return p, nil
}

// InvalidateProvider 在凭据被修改后清掉缓存。
func (m *Manager) InvalidateProvider(providerID int64) {
	m.mu.Lock()
	delete(m.providers, providerID)
	m.mu.Unlock()
}

// Apply 把选主结论落到 DNS 服务商上。
// manual=true 表示用户手动触发，会跳过冷却期限制。
func (m *Manager) Apply(ctx context.Context, rec *store.DNSRecord, d Decision, manual bool, cfg store.Settings) error {
	if d.TargetNodeID == 0 {
		return nil
	}

	if !manual && !d.Change {
		return nil
	}

	// 冷却期：防止节点状态在临界点抖动时域名被来回改。手动切换不受限制。
	if !manual && rec.LastSwitchAt != nil {
		cooldown := time.Duration(cfg.SwitchCooldownSec) * time.Second
		if cooldown > 0 && time.Since(*rec.LastSwitchAt) < cooldown {
			remain := cooldown - time.Since(*rec.LastSwitchAt)
			m.log.Info("DNS 切换处于冷却期，本次跳过",
				"record", rec.Name, "remain", remain.Round(time.Second))
			return nil
		}
	}

	p, err := m.provider(ctx, rec.ProviderID)
	if err != nil {
		m.recordFailure(ctx, rec, err)
		return err
	}

	rtype := rec.RecordType
	if rtype == "" {
		rtype = "A"
	}

	existing, err := p.List(ctx, rec.Zone, rec.Name, rtype)
	if err != nil {
		err = fmt.Errorf("查询现有解析记录: %w", err)
		m.recordFailure(ctx, rec, err)
		return err
	}

	target := dns.Record{
		Zone:    rec.Zone,
		Name:    rec.Name,
		Type:    rtype,
		Content: d.TargetValue,
		TTL:     rec.TTL,
		Proxied: rec.Proxied,
	}

	// 服务商侧已经是目标值了：不必再发写请求，但要把面板侧的记账补上
	// （常见于面板重启后第一次对账）。
	if len(existing) > 0 {
		target.ID = existing[0].ID
		target.Line = existing[0].Line
		if existing[0].Content == d.TargetValue {
			if err := m.st.SetDNSCurrent(ctx, rec.ID, d.TargetNodeID, d.TargetValue, time.Now().UTC()); err != nil {
				return err
			}
			m.log.Debug("解析值已是目标值，无需调用服务商接口",
				"record", rec.Name, "value", d.TargetValue)
			return nil
		}
	}

	prevValue := rec.CurrentValue
	if len(existing) > 0 {
		prevValue = existing[0].Content
	}

	if _, err := p.Upsert(ctx, target); err != nil {
		err = fmt.Errorf("写入解析记录: %w", err)
		m.recordFailure(ctx, rec, err)
		return err
	}

	now := time.Now().UTC()
	if err := m.st.SetDNSCurrent(ctx, rec.ID, d.TargetNodeID, d.TargetValue, now); err != nil {
		return err
	}
	rec.CurrentValue = d.TargetValue
	rec.CurrentNodeID = &d.TargetNodeID
	rec.LastSwitchAt = &now

	trigger := "自动故障切换"
	if manual {
		trigger = "手动切换"
	}
	msg := fmt.Sprintf("%s：%s 由 %s 改为 %s（节点「%s」）",
		trigger, rec.Name, orDash(prevValue), d.TargetValue, d.TargetNodeName)

	m.log.Info("DNS 切换完成", "record", rec.Name, "from", prevValue,
		"to", d.TargetValue, "node", d.TargetNodeName, "manual", manual)

	nodeID := d.TargetNodeID
	if err := m.st.AddEvent(ctx, &nodeID, store.EventDNSSwitch, store.LevelWarn, msg); err != nil {
		m.log.Warn("写入切换事件失败", "err", err)
	}
	m.notifier.Send(notify.Message{
		Level:    store.LevelWarn,
		Title:    "域名解析已切换",
		Body:     msg,
		NodeID:   d.TargetNodeID,
		NodeName: d.TargetNodeName,
	})

	return nil
}

func orDash(s string) string {
	if s == "" {
		return "(未记录)"
	}
	return s
}

func (m *Manager) recordFailure(ctx context.Context, rec *store.DNSRecord, err error) {
	m.log.Error("DNS 切换失败", "record", rec.Name, "err", err)
	if e := m.st.SetDNSError(ctx, rec.ID, err.Error()); e != nil {
		m.log.Warn("记录切换失败原因时出错", "err", e)
	}
	msg := fmt.Sprintf("域名 %s 切换失败：%v", rec.Name, err)
	if e := m.st.AddEvent(ctx, nil, store.EventDNSSwitch, store.LevelError, msg); e != nil {
		m.log.Warn("写入失败事件时出错", "err", e)
	}
	m.notifier.Send(notify.Message{
		Level: store.LevelError,
		Title: "域名解析切换失败",
		Body:  msg,
	})
}

// Reconcile 对所有启用了自动切换的记录做一轮选主并执行。
func (m *Manager) Reconcile(ctx context.Context) error {
	cfg, err := m.st.LoadSettings(ctx)
	if err != nil {
		return err
	}
	states, err := m.Snapshot(ctx)
	if err != nil {
		return err
	}
	for id, s := range states {
		m.MarkAlive(id, s.Alive, cfg.FailThreshold)
	}
	return m.ReconcileWith(ctx, states, cfg)
}

// ReconcileWith 用调用方已经采好的状态快照做对账。
// 调度器每轮只需要拨测一次，不必让 failover 再采一遍。
func (m *Manager) ReconcileWith(ctx context.Context, states map[int64]*NodeState, cfg store.Settings) error {
	records, err := m.st.ListDNSRecords(ctx)
	if err != nil {
		return err
	}

	var firstErr error
	for _, rec := range records {
		if !rec.Enabled || rec.Strategy != store.StrategyFailover {
			continue
		}
		d := m.Decide(rec, states, cfg)
		if !d.Change {
			continue
		}
		if err := m.Apply(ctx, rec, d, false, cfg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReconcileNode 只处理与某个节点相关的记录。
// 节点刚被判定超额或掉线时调用，比等下一轮全量对账快。
func (m *Manager) ReconcileNode(ctx context.Context, nodeID int64) error {
	cfg, err := m.st.LoadSettings(ctx)
	if err != nil {
		return err
	}
	states, err := m.Snapshot(ctx)
	if err != nil {
		return err
	}
	records, err := m.st.DNSRecordsForNode(ctx, nodeID)
	if err != nil {
		return err
	}

	var firstErr error
	for _, rec := range records {
		if !rec.Enabled || rec.Strategy != store.StrategyFailover {
			continue
		}
		d := m.Decide(rec, states, cfg)
		if !d.Change {
			continue
		}
		if err := m.Apply(ctx, rec, d, false, cfg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SwitchTo 手动把一条记录切到指定节点。忽略健康状态和冷却期——
// 用户点了按钮就是明确知道自己要干什么。
func (m *Manager) SwitchTo(ctx context.Context, recordID, nodeID int64) (Decision, error) {
	cfg, err := m.st.LoadSettings(ctx)
	if err != nil {
		return Decision{}, err
	}
	rec, err := m.st.GetDNSRecord(ctx, recordID)
	if err != nil {
		return Decision{}, err
	}
	node, err := m.st.GetNode(ctx, nodeID)
	if err != nil {
		return Decision{}, fmt.Errorf("目标节点不存在: %w", err)
	}

	value := recordValue(node, rec.RecordType)
	if value == "" {
		return Decision{}, fmt.Errorf("节点「%s」没有填写 %s 记录所需的 IP 地址", node.Name, rec.RecordType)
	}

	d := Decision{
		RecordID:       rec.ID,
		RecordName:     rec.Name,
		TargetNodeID:   node.ID,
		TargetNodeName: node.Name,
		TargetValue:    value,
		CurrentValue:   rec.CurrentValue,
		Change:         value != rec.CurrentValue,
	}

	if err := m.Apply(ctx, rec, d, true, cfg); err != nil {
		return d, err
	}
	return d, nil
}

// RemoteState 是从服务商查回来的记录真实状态。
type RemoteState struct {
	Found        bool   `json:"found"`
	Value        string `json:"value"`
	ProviderID   string `json:"provider_record_id"`
	MatchedNode  int64  `json:"matched_node_id"`
	MatchedName  string `json:"matched_node_name"`
	PanelValue   string `json:"panel_value"`
	Synchronized bool   `json:"synchronized"`
}

// FetchCurrent 从服务商拉取记录的真实解析值并同步回面板。
//
// 用户可能直接在服务商后台改过解析，面板记的"当前值"就会过时。
// 过时的当前值会让选主逻辑误判"不需要切换"，所以提供一个手动校准的入口。
func (m *Manager) FetchCurrent(ctx context.Context, rec *store.DNSRecord) (*RemoteState, error) {
	p, err := m.provider(ctx, rec.ProviderID)
	if err != nil {
		return nil, err
	}

	rtype := rec.RecordType
	if rtype == "" {
		rtype = "A"
	}
	records, err := p.List(ctx, rec.Zone, rec.Name, rtype)
	if err != nil {
		return nil, fmt.Errorf("查询解析记录: %w", err)
	}

	out := &RemoteState{PanelValue: rec.CurrentValue}
	if len(records) == 0 {
		return out, nil
	}

	out.Found = true
	out.Value = records[0].Content
	out.ProviderID = records[0].ID

	// 反查这个 IP 对应哪个节点，好让面板知道"现在指向谁"
	nodes, err := m.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if recordValue(n, rtype) == out.Value {
			out.MatchedNode, out.MatchedName = n.ID, n.Name
			break
		}
	}

	if out.MatchedNode > 0 {
		err = m.st.SetDNSCurrent(ctx, rec.ID, out.MatchedNode, out.Value, time.Now().UTC())
	} else {
		// IP 不属于任何已知节点：只记值，不记节点归属
		_, err = m.st.DB().ExecContext(ctx,
			`UPDATE dns_records SET current_value=?, current_node_id=NULL WHERE id=?`,
			out.Value, rec.ID)
	}
	if err != nil {
		return out, err
	}
	out.Synchronized = true

	return out, nil
}

// DryRun 返回所有记录的选主结论但不执行，用于首次配置时确认逻辑符合预期。
func (m *Manager) DryRun(ctx context.Context) ([]Decision, error) {
	cfg, err := m.st.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	states, err := m.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	records, err := m.st.ListDNSRecords(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Decision, 0, len(records))
	for _, rec := range records {
		d := m.Decide(rec, states, cfg)
		if !rec.Enabled {
			d.Skipped = "记录已禁用"
		} else if rec.Strategy != store.StrategyFailover {
			d.Skipped = "策略为手动，不参与自动切换"
		}
		out = append(out, d)
	}
	return out, nil
}
