package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// 设置项的 key。
const (
	KeyAdminUser     = "admin_user"
	KeyAdminPassHash = "admin_pass_hash"
	KeyJWTSecret     = "jwt_secret"

	KeyOfflineAfterSec   = "offline_after_sec"    // 心跳超过这个秒数判定离线
	KeyFailThreshold     = "fail_threshold"       // 连续失败几次才真的判定 down
	KeySwitchCooldownSec = "switch_cooldown_sec"  // DNS 两次切换之间的最小间隔
	KeyTCPProbeEnabled   = "tcp_probe_enabled"    // 探针失联后是否再做一次 TCP 拨测确认
	KeyTCPProbeTimeout   = "tcp_probe_timeout_ms" // 拨测超时
	KeyHourlyRetainDays  = "hourly_retain_days"   // 小时数据保留天数

	KeyTelegramToken  = "notify_telegram_token"
	KeyTelegramChatID = "notify_telegram_chat_id"
	KeyWebhookURL     = "notify_webhook_url"
)

// Settings 是运行期可调参数的集合（存在 settings 表，面板上可改）。
type Settings struct {
	OfflineAfterSec   int  `json:"offline_after_sec"`
	FailThreshold     int  `json:"fail_threshold"`
	SwitchCooldownSec int  `json:"switch_cooldown_sec"`
	TCPProbeEnabled   bool `json:"tcp_probe_enabled"`
	TCPProbeTimeoutMS int  `json:"tcp_probe_timeout_ms"`
	HourlyRetainDays  int  `json:"hourly_retain_days"`

	TelegramToken  string `json:"notify_telegram_token"`
	TelegramChatID string `json:"notify_telegram_chat_id"`
	WebhookURL     string `json:"notify_webhook_url"`
}

func DefaultSettings() Settings {
	return Settings{
		OfflineAfterSec:   90,
		FailThreshold:     3,
		SwitchCooldownSec: 300,
		TCPProbeEnabled:   true,
		TCPProbeTimeoutMS: 5000,
		HourlyRetainDays:  180,
	}
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) allSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// LoadSettings 读取运行期参数，缺失的项回落到默认值。
func (s *Store) LoadSettings(ctx context.Context) (Settings, error) {
	cfg := DefaultSettings()
	m, err := s.allSettings(ctx)
	if err != nil {
		return cfg, err
	}

	atoi := func(key string, dst *int) {
		if v, ok := m[key]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	atoi(KeyOfflineAfterSec, &cfg.OfflineAfterSec)
	atoi(KeyFailThreshold, &cfg.FailThreshold)
	atoi(KeySwitchCooldownSec, &cfg.SwitchCooldownSec)
	atoi(KeyTCPProbeTimeout, &cfg.TCPProbeTimeoutMS)
	atoi(KeyHourlyRetainDays, &cfg.HourlyRetainDays)

	if v, ok := m[KeyTCPProbeEnabled]; ok {
		cfg.TCPProbeEnabled = v == "1" || v == "true"
	}
	cfg.TelegramToken = m[KeyTelegramToken]
	cfg.TelegramChatID = m[KeyTelegramChatID]
	cfg.WebhookURL = m[KeyWebhookURL]

	return cfg, nil
}

func (s *Store) SaveSettings(ctx context.Context, cfg Settings) error {
	pairs := map[string]string{
		KeyOfflineAfterSec:   strconv.Itoa(cfg.OfflineAfterSec),
		KeyFailThreshold:     strconv.Itoa(cfg.FailThreshold),
		KeySwitchCooldownSec: strconv.Itoa(cfg.SwitchCooldownSec),
		KeyTCPProbeTimeout:   strconv.Itoa(cfg.TCPProbeTimeoutMS),
		KeyHourlyRetainDays:  strconv.Itoa(cfg.HourlyRetainDays),
		KeyTCPProbeEnabled:   boolStr(cfg.TCPProbeEnabled),
		KeyTelegramToken:     cfg.TelegramToken,
		KeyTelegramChatID:    cfg.TelegramChatID,
		KeyWebhookURL:        cfg.WebhookURL,
	}
	for k, v := range pairs {
		if err := s.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
