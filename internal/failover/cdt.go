package failover

import (
	"context"
	"fmt"
	"net"

	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/store"
)

// SyncCDTRecord uses ECS's current public address, independent of agent/node IPs.
// It bypasses ordinary failover priorities and cooldown, and verifies the remote
// records before allowing the rotation manager to schedule the old machine off.
func (m *Manager) SyncCDTRecord(ctx context.Context, recordID int64, ip string) error {
	rec, err := m.st.GetDNSRecord(ctx, recordID)
	if err != nil {
		return err
	}
	if !rec.Enabled || rec.Strategy != store.StrategyCDTRotation || rec.RecordType != "A" {
		return fmt.Errorf("DDNS 记录必须是已启用的 CDT 换班 A 记录")
	}
	if rec.TTL > 1800 {
		return fmt.Errorf("DNS TTL 超过 30 分钟，暂不停止旧实例")
	}
	if addr := net.ParseIP(ip); addr == nil || addr.To4() == nil {
		return fmt.Errorf("ECS 公网 IPv4 无效")
	}
	p, err := m.provider(ctx, rec.ProviderID)
	if err != nil {
		return err
	}
	rows, err := p.List(ctx, rec.Zone, rec.Name, "A")
	if err != nil {
		return err
	}
	changed := false
	if len(rows) == 0 {
		rows = []dns.Record{{Zone: rec.Zone, Name: rec.Name, Type: "A", TTL: rec.TTL, Proxied: rec.Proxied}}
	}
	for _, r := range rows {
		if r.Content == ip && r.TTL <= 1800 {
			continue
		}
		r.Content, r.TTL, r.Proxied = ip, rec.TTL, rec.Proxied
		if _, err := p.Upsert(ctx, r); err != nil {
			return fmt.Errorf("写入 DDNS：%w", err)
		}
		changed = true
	}
	if changed {
		rows, err = p.List(ctx, rec.Zone, rec.Name, "A")
		if err != nil {
			return fmt.Errorf("核实 DDNS：%w", err)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("DDNS 写入后未查询到 A 记录")
	}
	for _, r := range rows {
		if r.Content != ip || r.TTL > 1800 {
			return fmt.Errorf("DDNS 远端记录尚未与目标 IP/TTL 一致")
		}
	}
	if changed || rec.CurrentValue != ip || rec.CurrentNodeID != nil {
		if err := m.st.SetDNSAddress(ctx, rec.ID, ip); err != nil {
			return err
		}
		m.log.Info("CDT DDNS 已核实", "record", rec.Name, "ip", ip)
	}
	return nil
}
