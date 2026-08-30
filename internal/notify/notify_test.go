package notify

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramTextKeepsSingleMessageFormat(t *testing.T) {
	got := telegramText([]Message{{
		Level: store.LevelWarn, Title: "节点离线", Body: "探针失联", NodeName: "香港",
	}})
	want := "🟡 节点离线\n节点: 香港\n探针失联"
	if got != want {
		t.Fatalf("单条通知格式发生变化\n实际: %q\n期望: %q", got, want)
	}
}

func TestTelegramTextSummarizesSameEvent(t *testing.T) {
	got := telegramText([]Message{
		{Level: store.LevelWarn, Title: "节点离线", Body: "香港离线", NodeName: "香港"},
		{Level: store.LevelWarn, Title: "节点离线", Body: "东京离线", NodeName: "东京"},
		{Level: store.LevelWarn, Title: "节点离线", Body: "新加坡离线", NodeName: "新加坡"},
	})
	for _, want := range []string{"节点离线（3 条汇总）", "1. 节点: 香港", "2. 节点: 东京", "3. 节点: 新加坡"} {
		if !strings.Contains(got, want) {
			t.Errorf("汇总通知缺少 %q：\n%s", want, got)
		}
	}
}

func TestQueueTelegramBatchesSameEventWithinWindow(t *testing.T) {
	requests := make(chan url.Values, 2)
	n := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.batchWindow = 40 * time.Millisecond
	n.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		requests <- form
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	cfg := store.Settings{TelegramToken: "test-token", TelegramChatID: "123"}
	for _, node := range []string{"香港", "东京", "新加坡"} {
		n.queueTelegram(cfg, Message{
			Level: store.LevelWarn, Title: "节点离线", Body: node + "离线", NodeName: node,
		})
	}

	var form url.Values
	select {
	case form = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Telegram 汇总发送超时")
	}
	if got := form.Get("chat_id"); got != "123" {
		t.Errorf("chat_id = %q，期望 123", got)
	}
	if got := form.Get("text"); !strings.Contains(got, "节点离线（3 条汇总）") {
		t.Errorf("没有合成一条三项通知：\n%s", got)
	}

	select {
	case extra := <-requests:
		t.Fatalf("同一事件 10 秒窗口内不应发送第二次请求：%v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}
