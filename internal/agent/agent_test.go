package agent

import (
	"errors"
	"testing"
)

func TestParseServer(t *testing.T) {
	cases := []struct {
		in         string
		wantHost   string
		wantSecure bool
		wantFixed  bool
	}{
		// 写明协议的一律照办，且不允许后续自动翻转
		{"https://panel.example.com", "panel.example.com", true, true},
		{"http://panel.example.com", "panel.example.com", false, true},
		{"wss://panel.example.com", "panel.example.com", true, true},
		{"ws://1.2.3.4:8080", "1.2.3.4:8080", false, true},
		{"http://1.2.3.4:8080/", "1.2.3.4:8080", false, true},

		// 没写协议时的推断。这正是用户踩到的场景：
		// 面板跑在 152.67.6.102:8080 上，是明文 HTTP。
		{"152.67.6.102:8080", "152.67.6.102:8080", false, false},
		{"1.2.3.4", "1.2.3.4", false, false},
		{"1.2.3.4:9999", "1.2.3.4:9999", false, false},
		{"panel.example.com", "panel.example.com", true, false},
		{"panel.example.com:443", "panel.example.com:443", true, false},
		{"panel.example.com:8080", "panel.example.com:8080", false, false},
		{"1.2.3.4:443", "1.2.3.4:443", true, false},

		// 空白和结尾斜杠要吃掉
		{"  http://a.com/  ", "a.com", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			host, secure, fixed := parseServer(tc.in)
			if host != tc.wantHost {
				t.Errorf("host = %q，期望 %q", host, tc.wantHost)
			}
			if secure != tc.wantSecure {
				t.Errorf("secure = %v，期望 %v", secure, tc.wantSecure)
			}
			if fixed != tc.wantFixed {
				t.Errorf("fixed = %v，期望 %v", fixed, tc.wantFixed)
			}
		})
	}
}

func TestSchemeMismatch(t *testing.T) {
	// 用户实际遇到的那条错误
	realErr := errors.New(`Post "https://152.67.6.102:8080/agent/report": ` +
		`http: server gave HTTP response to HTTPS client`)
	if !schemeMismatch(realErr) {
		t.Error("应识别出「明文服务端被当成 HTTPS」这种协议错配")
	}

	// 反方向：明文客户端连了 TLS 服务端
	if !schemeMismatch(errors.New(`malformed HTTP response "\x15\x03\x01"`)) {
		t.Error("应识别出「TLS 服务端被当成明文」")
	}

	// 普通网络错误不该被当成协议问题去翻转
	for _, e := range []error{
		nil,
		errors.New("dial tcp 1.2.3.4:8080: connect: connection refused"),
		errors.New("context deadline exceeded"),
		errors.New("面板拒绝了节点密钥"),
	} {
		if schemeMismatch(e) {
			t.Errorf("不该判为协议错配: %v", e)
		}
	}
}

func TestMaybeFlipScheme(t *testing.T) {
	protoErr := errors.New("http: server gave HTTP response to HTTPS client")

	t.Run("推断出来的协议可以翻转", func(t *testing.T) {
		a := newTestAgent(t, "152.67.6.102:8080")
		if a.secure.Load() {
			t.Fatal("裸 IP + 8080 应先猜明文")
		}
		// 先人为设成 https，模拟猜错
		a.secure.Store(true)
		if !a.maybeFlipScheme(protoErr) {
			t.Fatal("协议错配时应翻转")
		}
		if a.secure.Load() {
			t.Error("翻转后应变成明文")
		}
		if got := a.httpEndpoint(); got != "http://152.67.6.102:8080/agent/report" {
			t.Errorf("上报地址 = %q", got)
		}
		if got := a.wsEndpoint(); got != "ws://152.67.6.102:8080/agent/ws" {
			t.Errorf("WebSocket 地址 = %q", got)
		}
	})

	t.Run("用户写死的协议不动", func(t *testing.T) {
		a := newTestAgent(t, "https://panel.example.com")
		if !a.schemeFixed {
			t.Fatal("显式写了 https:// 应标记为固定")
		}
		if a.maybeFlipScheme(protoErr) {
			t.Error("用户明确指定的协议不该被偷偷改掉")
		}
		if !a.secure.Load() {
			t.Error("协议不该变")
		}
	})

	t.Run("普通错误不翻转", func(t *testing.T) {
		a := newTestAgent(t, "1.2.3.4:8080")
		before := a.secure.Load()
		if a.maybeFlipScheme(errors.New("connection refused")) {
			t.Error("普通网络错误不该触发翻转")
		}
		if a.secure.Load() != before {
			t.Error("协议不该变")
		}
	})
}

func TestEndpoints(t *testing.T) {
	a := newTestAgent(t, "https://panel.example.com")
	if got := a.wsEndpoint(); got != "wss://panel.example.com/agent/ws" {
		t.Errorf("wsEndpoint = %q", got)
	}
	if got := a.httpEndpoint(); got != "https://panel.example.com/agent/report" {
		t.Errorf("httpEndpoint = %q", got)
	}
}

func newTestAgent(t *testing.T, server string) *Agent {
	t.Helper()
	a, err := New(Config{Server: server, Secret: "test-secret"})
	if err != nil {
		t.Fatalf("构造探针: %v", err)
	}
	return a
}
