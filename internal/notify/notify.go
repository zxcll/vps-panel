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
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

// telegramBatchWindow 是同类 Telegram 通知的汇总窗口。
//
// 以第一条为起点固定等 10 秒：这段时间里 Level + Title 相同的通知只发一条，
// 每个原始通知作为一项列在正文里。Webhook 保持原来的逐条实时投递语义。
const telegramBatchWindow = 10 * time.Second

type telegramBatchKey struct {
	token  string
	chatID string
	level  string
	title  string
}

type telegramBatch struct {
	cfg      store.Settings
	messages []Message
}

type Notifier struct {
	st     *store.Store
	client *http.Client
	log    *slog.Logger

	telegramMu      sync.Mutex
	telegramBatches map[telegramBatchKey]*telegramBatch
	batchWindow     time.Duration
}

func New(st *store.Store, log *slog.Logger) *Notifier {
	return &Notifier{
		st:              st,
		client:          &http.Client{Timeout: 15 * time.Second},
		log:             log,
		telegramBatches: make(map[telegramBatchKey]*telegramBatch),
		batchWindow:     telegramBatchWindow,
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
			n.queueTelegram(cfg, msg)
		}
		if cfg.WebhookURL != "" {
			if err := n.sendWebhook(ctx, cfg.WebhookURL, msg); err != nil {
				n.log.Warn("Webhook 通知发送失败", "err", err)
			}
		}
	}()
}

// queueTelegram 把同一目的地、同一级别、同一标题的通知放进一个 10 秒批次。
// 定时器从第一条开始计时而不是每来一条就顺延，持续抖动时也不会永远发不出去。
func (n *Notifier) queueTelegram(cfg store.Settings, msg Message) {
	key := telegramBatchKey{
		token: cfg.TelegramToken, chatID: cfg.TelegramChatID,
		level: msg.Level, title: msg.Title,
	}

	n.telegramMu.Lock()
	if batch := n.telegramBatches[key]; batch != nil {
		batch.messages = append(batch.messages, msg)
		n.telegramMu.Unlock()
		return
	}
	n.telegramBatches[key] = &telegramBatch{cfg: cfg, messages: []Message{msg}}
	window := n.batchWindow
	if window <= 0 {
		window = telegramBatchWindow
	}
	time.AfterFunc(window, func() { n.flushTelegram(key) })
	n.telegramMu.Unlock()
}

func (n *Notifier) flushTelegram(key telegramBatchKey) {
	n.telegramMu.Lock()
	batch := n.telegramBatches[key]
	delete(n.telegramBatches, key)
	n.telegramMu.Unlock()
	if batch == nil || len(batch.messages) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.sendTelegram(ctx, batch.cfg, batch.messages); err != nil {
		n.log.Warn("Telegram 通知发送失败", "err", err, "汇总条数", len(batch.messages))
	}
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

func telegramText(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	first := messages[0]
	var sb strings.Builder
	sb.WriteString(levelEmoji(first.Level))
	sb.WriteString(" ")
	sb.WriteString(first.Title)

	if len(messages) == 1 {
		if first.NodeName != "" {
			sb.WriteString("\n节点: ")
			sb.WriteString(first.NodeName)
		}
		if first.Body != "" {
			sb.WriteString("\n")
			sb.WriteString(first.Body)
		}
		return sb.String()
	}

	fmt.Fprintf(&sb, "（%d 条汇总）", len(messages))
	for i, msg := range messages {
		fmt.Fprintf(&sb, "\n\n%d.", i+1)
		if msg.NodeName != "" {
			sb.WriteString(" 节点: ")
			sb.WriteString(msg.NodeName)
		}
		if msg.Body != "" {
			if msg.NodeName == "" {
				sb.WriteString(" ")
			} else {
				sb.WriteString("\n")
			}
			sb.WriteString(msg.Body)
		}
	}
	return sb.String()
}

func (n *Notifier) sendTelegram(ctx context.Context, cfg store.Settings, messages []Message) error {
	text := telegramText(messages)
	if text == "" {
		return nil
	}

	form := url.Values{}
	form.Set("chat_id", cfg.TelegramChatID)
	form.Set("text", text)
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
