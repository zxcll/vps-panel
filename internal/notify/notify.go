// Package notify 把重要事件推送给用户（Telegram / 自定义 Webhook）。
//
// 通知是尽力而为的旁路：推送失败绝不能影响关机、切 DNS 这些主流程，
// 所以这里所有错误都只写日志，不向上抛。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

type Notifier struct {
	st     *store.Store
	client *http.Client
	log    *slog.Logger
}

func New(st *store.Store, log *slog.Logger) *Notifier {
	return &Notifier{
		st:     st,
		client: &http.Client{Timeout: 15 * time.Second},
		log:    log,
	}
}

// Message 是一条通知。
type Message struct {
	Level    string `json:"level"` // info|warn|error
	Title    string `json:"title"`
	Body     string `json:"body"`
	NodeID   int64  `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	Time     string `json:"time"`
}

// Send 异步推送一条通知。调用方不需要等待，也不需要处理错误。
func (n *Notifier) Send(msg Message) {
	if n == nil {
		return
	}
	msg.Time = time.Now().UTC().Format(time.RFC3339)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cfg, err := n.st.LoadSettings(ctx)
		if err != nil {
			n.log.Warn("读取通知配置失败", "err", err)
			return
		}

		if cfg.TelegramToken != "" && cfg.TelegramChatID != "" {
			if err := n.sendTelegram(ctx, cfg, msg); err != nil {
				n.log.Warn("Telegram 通知发送失败", "err", err)
			}
		}
		if cfg.WebhookURL != "" {
			if err := n.sendWebhook(ctx, cfg.WebhookURL, msg); err != nil {
				n.log.Warn("Webhook 通知发送失败", "err", err)
			}
		}
	}()
}

func levelEmoji(level string) string {
	switch level {
	case store.LevelError:
		return "🔴"
	case store.LevelWarn:
		return "🟡"
	default:
		return "🔵"
	}
}

func (n *Notifier) sendTelegram(ctx context.Context, cfg store.Settings, msg Message) error {
	var sb strings.Builder
	sb.WriteString(levelEmoji(msg.Level))
	sb.WriteString(" ")
	sb.WriteString(msg.Title)
	if msg.NodeName != "" {
		sb.WriteString("\n节点: ")
		sb.WriteString(msg.NodeName)
	}
	if msg.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(msg.Body)
	}

	form := url.Values{}
	form.Set("chat_id", cfg.TelegramChatID)
	form.Set("text", sb.String())
	form.Set("disable_web_page_preview", "true")

	endpoint := "https://api.telegram.org/bot" + cfg.TelegramToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Telegram 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (n *Notifier) sendWebhook(ctx context.Context, endpoint string, msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Webhook 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
