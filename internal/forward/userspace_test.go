package forward

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// freePort 挑一个当前空闲的端口号。
// 先绑 :0 拿到内核分配的号再放掉，理论上有竞争窗口，但测试里够用了。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("挑空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// startEchoServer 起一个把收到的内容原样回吐的上游。
func startEchoServer(t *testing.T) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起上游失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// startGreetingServer 起一个"服务端先说话"的上游（SSH / SMTP / FTP 都是这样）。
// 连上来就先吐一段横幅，之后再回显。
func startGreetingServer(t *testing.T, banner string) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起上游失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte(banner))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func dialAndRoundTrip(t *testing.T, port int, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("连转发端口失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	return string(buf)
}

func TestUserspaceRelayRoundTrip(t *testing.T) {
	upIP, upPort := startEchoServer(t)
	listenPort := freePort(t)

	b := newUserspaceBackend(0) // 关掉预连接池，先测最裸的路径
	t.Cleanup(b.Close)

	rule := Rule{HopID: 1, Proto: ProtoTCP, ListenPort: listenPort, DestIP: upIP, DestPort: upPort}
	if err := b.Reconcile([]Rule{rule}); err != nil {
		t.Fatalf("起监听失败: %v", err)
	}

	if got := dialAndRoundTrip(t, listenPort, "hello relay"); got != "hello relay" {
		t.Errorf("回显内容 = %q，期望 %q", got, "hello relay")
	}

	// 上下行都要计数，而且是在连接跑着的时候就能看到（不是等关闭才结算）。
	var c Counter
	for _, x := range b.Counters() {
		if x.HopID == 1 {
			c = x
		}
	}
	if c.BytesUp != int64(len("hello relay")) {
		t.Errorf("上行计数 = %d，期望 %d", c.BytesUp, len("hello relay"))
	}
	if c.BytesDown != int64(len("hello relay")) {
		t.Errorf("下行计数 = %d，期望 %d", c.BytesDown, len("hello relay"))
	}
}

// 这条是连接池最容易写错的地方：探活必须用 MSG_PEEK，
// 普通 Read 会把上游已经发来的第一个字节吃掉并丢弃，
// 于是所有"服务端先说话"的协议（SSH、SMTP、FTP 横幅）都会坏掉。
func TestPooledConnKeepsServerFirstByte(t *testing.T) {
	const banner = "SSH-2.0-OpenSSH_9.2\r\n"
	upIP, upPort := startGreetingServer(t, banner)
	listenPort := freePort(t)

	b := newUserspaceBackend(4) // 开预连接池
	t.Cleanup(b.Close)

	rule := Rule{HopID: 1, Proto: ProtoTCP, ListenPort: listenPort, DestIP: upIP, DestPort: upPort}
	if err := b.Reconcile([]Rule{rule}); err != nil {
		t.Fatalf("起监听失败: %v", err)
	}

	// 等池子把连接建起来 —— 这些连接里已经躺着上游发来的横幅了，
	// 正是探活最容易把它吃掉的时刻。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l := b.listeners[listenPort]; l != nil {
			if p := l.pool.Load(); p != nil && len(p.idle) > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 连续几次，确保用到的是池里的连接而不是临时拨的。
	for i := range 3 {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)), 3*time.Second)
		if err != nil {
			t.Fatalf("第 %d 次连接失败: %v", i+1, err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, len(banner))
		if _, err := io.ReadFull(c, buf); err != nil {
			c.Close()
			t.Fatalf("第 %d 次读横幅失败: %v", i+1, err)
		}
		if string(buf) != banner {
			c.Close()
			t.Fatalf("第 %d 次收到的横幅 = %q，期望 %q（首字节被探活吃掉了）", i+1, string(buf), banner)
		}
		c.Close()
	}
}

func TestReconcileHotUpdatesTarget(t *testing.T) {
	up1IP, up1Port := startGreetingServer(t, "ONE")
	up2IP, up2Port := startGreetingServer(t, "TWO")
	listenPort := freePort(t)

	b := newUserspaceBackend(0)
	t.Cleanup(b.Close)

	rule := Rule{HopID: 1, Proto: ProtoTCP, ListenPort: listenPort, DestIP: up1IP, DestPort: up1Port}
	if err := b.Reconcile([]Rule{rule}); err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	if got := readBanner(t, listenPort, 3); got != "ONE" {
		t.Fatalf("初始目标错了：%q", got)
	}

	// 改目标不该重启监听（端口不能有中断），只热换 target。
	rule.DestIP, rule.DestPort = up2IP, up2Port
	if err := b.Reconcile([]Rule{rule}); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if got := readBanner(t, listenPort, 3); got != "TWO" {
		t.Errorf("热更新后目标 = %q，期望 TWO", got)
	}
}

func TestReconcileReplacesListenerWhenHopChanges(t *testing.T) {
	upIP, upPort := startEchoServer(t)
	listenPort := freePort(t)

	b := newUserspaceBackend(0)
	t.Cleanup(b.Close)

	if err := b.Reconcile([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: listenPort, DestIP: upIP, DestPort: upPort},
	}); err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	dialAndRoundTrip(t, listenPort, "abcdefgh")

	// 同一个端口换成另一跳：必须重建监听器，否则上一条规则的字节
	// 会被算到新规则头上。
	if err := b.Reconcile([]Rule{
		{HopID: 2, Proto: ProtoTCP, ListenPort: listenPort, DestIP: upIP, DestPort: upPort},
	}); err != nil {
		t.Fatalf("换跳失败: %v", err)
	}
	for _, c := range b.Counters() {
		if c.HopID == 2 && (c.BytesUp != 0 || c.BytesDown != 0) {
			t.Errorf("新跳的计数应从 0 开始，实际 %d/%d", c.BytesUp, c.BytesDown)
		}
		if c.HopID == 1 {
			t.Error("旧跳的监听器应该已经被关掉")
		}
	}
}

func TestReconcileRollsBackOnBindFailure(t *testing.T) {
	upIP, upPort := startEchoServer(t)
	goodPort := freePort(t)

	// 先占住一个端口，让后面绑它的规则必然失败。
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer blocker.Close()
	takenPort := blocker.Addr().(*net.TCPAddr).Port

	b := newUserspaceBackend(0)
	t.Cleanup(b.Close)

	err = b.Reconcile([]Rule{
		{HopID: 1, Proto: ProtoTCP, ListenPort: goodPort, DestIP: upIP, DestPort: upPort},
		{HopID: 2, Proto: ProtoTCP, ListenPort: takenPort, DestIP: upIP, DestPort: upPort},
	})
	if err == nil {
		t.Fatal("端口被占用时应该报错")
	}
	// 这一轮刚开的监听器要全部回滚掉，不能留下半套状态。
	if len(b.listeners) != 0 {
		t.Errorf("失败后应回滚，实际还剩 %d 个监听器", len(b.listeners))
	}
	if c, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", goodPort), 300*time.Millisecond); derr == nil {
		c.Close()
		t.Error("回滚后 goodPort 不该还在监听")
	}
}

func readBanner(t *testing.T, port, n int) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读横幅失败: %v", err)
	}
	return string(buf)
}
