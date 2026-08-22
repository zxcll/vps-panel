package agent

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// MeasureRTT 是为了解决这个问题存在的：拨下一跳的**转发端口**量不到本段延迟，
// 因为内核态转发在包进协议栈之前就把目标地址改写转发走了，下一跳自己不接这个
// 连接 —— 握手是和更下游完成的。所以本段延迟必须单独拨一个会在本地终结的端口。

func newRTTForwarder(t *testing.T) *forwarder {
	t.Helper()
	return newTestForwarder(t, filepath.Join(t.TempDir(), "forward.json"))
}

// 端口上有人在听：正常量到往返时间。
func TestMeasureRTTOnOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	f := newRTTForwarder(t)
	ms, err := f.MeasureRTT(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("应当量得到: %v", err)
	}
	if ms < 0 {
		t.Errorf("往返时间不该是负数：%d", ms)
	}
}

// 这条是关键：**端口关着照样是一次有效测量**。
//
// RST 是对方内核直接回的，往返时间和握手成功时一样准。要是把「连接被拒绝」
// 当成失败，那就只能挑一个恰好有服务在听的端口去拨 —— 而中转机上未必有。
func TestMeasureRTTTreatsRefusedAsValid(t *testing.T) {
	// 先占一个端口再放掉，拿到一个几乎肯定没人听的地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	f := newRTTForwarder(t)
	ms, err := f.MeasureRTT(context.Background(), addr)
	if err != nil {
		t.Fatalf("连接被拒绝也该算量到了（RST 同样是一次往返），实际报错: %v", err)
	}
	if ms < 0 {
		t.Errorf("往返时间不该是负数：%d", ms)
	}
}

// 拨不通（超时、被静默丢弃）就如实报错，不能编一个数字出来 ——
// 显示成 0ms 会让人以为这一段是零延迟。
//
// 这里用「已取消的 context」来制造失败，而不是拨一个不可路由的地址：
// 不同环境对 192.0.2.1 这类地址的反应不一样（有的超时，有的当场
// 回 ECONNREFUSED），拿它当断言会让用例时灵时不灵。
func TestMeasureRTTReportsFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := newRTTForwarder(t)
	ms, err := f.MeasureRTT(ctx, ln.Addr().String())
	if err == nil {
		t.Fatalf("拨不通时应当报错，实际返回 %d ms", ms)
	}
	if ms != 0 {
		t.Errorf("没量到就该返回 0，实际 %d", ms)
	}
}

// 超时也走报错这条路。
func TestMeasureRTTReportsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	f := newRTTForwarder(t)
	if ms, err := f.MeasureRTT(ctx, "127.0.0.1:1"); err == nil {
		t.Errorf("超时应当报错，实际返回 %d ms", ms)
	}
}

// 空地址表示不用测，安静返回。
func TestMeasureRTTSkipsEmptyTarget(t *testing.T) {
	f := newRTTForwarder(t)
	ms, err := f.MeasureRTT(context.Background(), "")
	if err != nil || ms != 0 {
		t.Errorf("空地址应当安静跳过，实际 %d ms, err=%v", ms, err)
	}
}
