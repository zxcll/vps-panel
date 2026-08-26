package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zxcll/vps-panel/internal/store"
)

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{
		Level: q.Get("level"),
		Type:  q.Get("type"),
		Limit: atoiDefault(q.Get("limit"), 200),
	}
	if v := q.Get("node_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "node_id 不合法")
			return
		}
		f.NodeID = &id
	}

	events, err := s.st.ListEvents(r.Context(), f)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.st.LoadSettings(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	// Telegram Token 属于凭据，不回显完整值
	cfg.TelegramToken = maskSecret(cfg.TelegramToken)
	writeJSON(w, http.StatusOK, cfg)
}

// maskSecret 只保留首尾几位，中间打码。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "********"
	}
	return s[:4] + "********" + s[len(s)-4:]
}

type settingsRequest struct {
	store.Settings
	// TelegramTokenChanged 为 true 时才写入 Token，
	// 否则保留旧值（因为 GET 返回的是打码后的假值）。
	TelegramTokenChanged bool `json:"telegram_token_changed"`
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	cfg := req.Settings
	if err := validateSettings(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if !req.TelegramTokenChanged {
		old, _ := s.st.GetSetting(ctx, store.KeyTelegramToken)
		cfg.TelegramToken = old
	}

	if err := s.st.SaveSettings(ctx, cfg); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.st.AddEvent(ctx, nil, store.EventSystem, store.LevelInfo, "面板设置已更新")

	cfg.TelegramToken = maskSecret(cfg.TelegramToken)
	writeJSON(w, http.StatusOK, cfg)
}

func validateSettings(cfg *store.Settings) error {
	if cfg.OfflineAfterSec < 30 {
		return fmt.Errorf("离线判定阈值不能低于 30 秒，否则正常的网络抖动就会被当成掉线")
	}
	if cfg.OfflineAfterSec > 3600 {
		return fmt.Errorf("离线判定阈值不能超过 3600 秒")
	}
	if cfg.FailThreshold < 1 || cfg.FailThreshold > 20 {
		return fmt.Errorf("连续失败次数需要在 1 ~ 20 之间")
	}
	if cfg.SwitchCooldownSec < 0 || cfg.SwitchCooldownSec > 86400 {
		return fmt.Errorf("切换冷却时间需要在 0 ~ 86400 秒之间")
	}
	if cfg.TCPProbeTimeoutMS < 500 || cfg.TCPProbeTimeoutMS > 60000 {
		return fmt.Errorf("拨测超时需要在 500 ~ 60000 毫秒之间")
	}
	if cfg.HourlyRetainDays < 7 || cfg.HourlyRetainDays > 3650 {
		return fmt.Errorf("流量曲线保留天数需要在 7 ~ 3650 之间")
	}
	// 加速前缀会被拼在 GitHub 地址前面，不是 http(s) 开头的话拼出来一定不对，
	// 而失败要等到点「检查更新」才暴露，不如现在就挡住。
	cfg.GitHubProxy = strings.TrimSpace(cfg.GitHubProxy)
	if cfg.GitHubProxy != "" &&
		!strings.HasPrefix(cfg.GitHubProxy, "http://") &&
		!strings.HasPrefix(cfg.GitHubProxy, "https://") {
		return fmt.Errorf("GitHub 加速前缀 %q 不合法，要以 http:// 或 https:// 开头（如 https://ghfast.top/）",
			cfg.GitHubProxy)
	}
	return nil
}
