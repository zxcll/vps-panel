package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// maxConnsPerPort 限制单个端口的并发 goroutine 数，
// 免得一次连接洪水把内存撑爆。超出的连接会在 Accept 处等名额（背压）。
const maxConnsPerPort = 1024

// target 包一层是为了能用 atomic.Pointer 整体换掉，
// 避免热更新目标地址时读到半新半旧的字段。
type target struct{ addr string }

// listener 是一条用户态 TCP 转发：一个 net.Listener，加上所有连接共享的
// 拨号目标和限速器。目标和限速都能在不重启监听的前提下热更新。
type listener struct {
	hopID int64
	port  int
	ln    net.Listener

	tgt atomic.Pointer[target]
	// lim 上下行共用同一个令牌桶：中转的两个方向都是本机出方向，
	// 内核态那边 tc 也是这么算的，两种模式下同一个数字得是同一个意思。
	lim       atomic.Pointer[rate.Limiter]
	bytesUp   atomic.Int64
	bytesDown atomic.Int64

	pool     atomic.Pointer[connPool]
	poolSize int

	sem    chan struct{} // 计数信号量，容量 maxConnsPerPort
	conns  sync.Map      // net.Conn -> struct{}，只为了 close() 能把在途连接拆掉
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func openListener(r Rule, poolSize int) (*listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.ListenPort))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &listener{
		hopID:    r.HopID,
		port:     r.ListenPort,
		ln:       ln,
		ctx:      ctx,
		cancel:   cancel,
		sem:      make(chan struct{}, maxConnsPerPort),
		poolSize: poolSize,
	}
	addr := r.Target()
	l.tgt.Store(&target{addr: addr})
	l.lim.Store(newLimiter(r.BandwidthMbps))
	if poolSize > 0 && addr != "" {
		l.pool.Store(newConnPool(addr, poolSize))
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

func (l *listener) acceptLoop() {
	defer l.wg.Done()
	var backoff time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // 监听被关掉了，正常退出
			}
			// 临时性的 accept 错误（fd 耗尽、ECONNABORTED）不该让这个端口永久失效。
			// 退避重试，条件恢复后监听自己就活过来了，不用等下一次 Reconcile。
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else {
				backoff *= 2
			}
			if backoff > time.Second {
				backoff = time.Second
			}
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		setKeepAlive(conn)

		// 背压：先抢到名额再开 goroutine，保证并发数不超过 maxConnsPerPort。
		select {
		case l.sem <- struct{}{}:
		case <-l.ctx.Done():
			conn.Close()
			return
		}
		l.wg.Add(1)
		go func() {
			defer func() { <-l.sem }()
			defer l.wg.Done()
			l.handle(conn)
		}()
	}
}

func (l *listener) handle(client net.Conn) {
	l.conns.Store(client, struct{}{})
	defer func() { l.conns.Delete(client); client.Close() }()

	tgt := l.tgt.Load()
	if tgt == nil || tgt.addr == "" {
		return // 域名还没解析出来，这条规则暂时不通
	}

	var upstream net.Conn
	var err error
	if p := l.pool.Load(); p != nil {
		upstream, err = p.Get()
	} else {
		upstream, err = dialUpstream(tgt.addr)
	}
	if err != nil {
		return
	}
	l.conns.Store(upstream, struct{}{})
	defer func() { l.conns.Delete(upstream); upstream.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		relayCopy(l.ctx, upstream, client, &l.lim, &l.bytesUp)
		halfCloseWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		relayCopy(l.ctx, client, upstream, &l.lim, &l.bytesDown)
		halfCloseWrite(client)
		done <- struct{}{}
	}()

	<-done
	// 一个方向结束了（连接进入半关闭）。给另一个方向一个上限：
	// 卡在 FIN_WAIT2 的对端不会被 keepalive 探测，不设上限的话
	// 这个 goroutine 和它占的信号量名额就永久挂住了。
	// 强行关掉两端能把卡住的拷贝唤醒，和 close() 用的是同一套无竞争的拆除手法。
	timer := time.AfterFunc(relayLinger, func() {
		client.Close()
		upstream.Close()
	})
	<-done
	timer.Stop()
}

// close 停止接受新连接，强拆在途连接，等所有 goroutine 退出。
// 不做优雅排空 —— 转发层刻意做得很薄。
func (l *listener) close() {
	l.cancel()
	_ = l.ln.Close()
	if p := l.pool.Load(); p != nil {
		p.Close()
	}
	l.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
	l.wg.Wait()
}

// userspaceBackend 按端口维护一组监听器。
type userspaceBackend struct {
	mu        sync.Mutex
	listeners map[int]*listener
	poolSize  int
}

func newUserspaceBackend(poolSize int) *userspaceBackend {
	return &userspaceBackend{listeners: map[int]*listener{}, poolSize: poolSize}
}

// Reconcile 让运行中的监听器集合与 rules 一致。
//
// 顺序是「先开新的、再改存量、最后关掉多余的」（make-before-break）：
// 中间某个端口绑不上时，把这一轮刚开的都回滚掉，上一版监听器完好无损，
// 转发不会因为一条规则写错就整体中断。
func (b *userspaceBackend) Reconcile(rules []Rule) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	desired := make(map[int]Rule, len(rules))
	for _, r := range rules {
		desired[r.ListenPort] = r
	}

	var opened []*listener
	for port, r := range desired {
		if existing, ok := b.listeners[port]; ok {
			// 端口还在用，但如果换成了另一跳，计数就必须重来 ——
			// 沿用旧监听器会把上一条规则的字节算到新规则头上。
			if existing.hopID == r.HopID {
				continue
			}
			existing.close()
			delete(b.listeners, port)
		}
		l, err := openListener(r, b.poolSize)
		if err != nil {
			for _, ol := range opened {
				ol.close()
				delete(b.listeners, ol.port)
			}
			return fmt.Errorf("监听 tcp/%d 失败: %w", port, err)
		}
		b.listeners[port] = l
		opened = append(opened, l)
	}

	// 热更新目标和限速。
	for port, r := range desired {
		l := b.listeners[port]
		newAddr := r.Target()
		old := l.tgt.Load()
		l.tgt.Store(&target{addr: newAddr})
		l.lim.Store(newLimiter(r.BandwidthMbps))

		// 目标变了，池里预建的连接全指向旧地址，得整个换掉。
		if b.poolSize > 0 && (old == nil || old.addr != newAddr) {
			if p := l.pool.Load(); p != nil {
				p.Close()
			}
			if newAddr != "" {
				l.pool.Store(newConnPool(newAddr, b.poolSize))
			} else {
				l.pool.Store(nil)
			}
		}
	}

	for port, l := range b.listeners {
		if _, ok := desired[port]; !ok {
			l.close()
			delete(b.listeners, port)
		}
	}
	return nil
}

func (b *userspaceBackend) Counters() []Counter {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Counter, 0, len(b.listeners))
	for _, l := range b.listeners {
		out = append(out, Counter{
			HopID:     l.hopID,
			BytesUp:   l.bytesUp.Load(),
			BytesDown: l.bytesDown.Load(),
		})
	}
	return out
}

func (b *userspaceBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for port, l := range b.listeners {
		l.close()
		delete(b.listeners, port)
	}
}

// ListenPorts 返回当前所有用户态监听端口，防火墙垫片要用它放行入站。
func (b *userspaceBackend) ListenPorts() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]int, 0, len(b.listeners))
	for port := range b.listeners {
		out = append(out, port)
	}
	return out
}
