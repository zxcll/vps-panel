package forward

import (
	"context"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// DefaultPoolSize 是每个用户态监听端口预建的连接数。设 0 关闭预连接。
	DefaultPoolSize = 4

	poolRetryBackoff = 2 * time.Second
	// poolConnMaxAge 限制预建连接的存活时间。挂太久的连接很可能已经被
	// 中间的 NAT/防火墙悄悄回收了，宁可重建也不要拿去服务真实请求。
	poolConnMaxAge = 90 * time.Second
	// poolFillInterval 是池满之后的巡检间隔。
	poolFillInterval = 500 * time.Millisecond
)

type timedConn struct {
	net.Conn
	created time.Time
}

// connPool 维护到单个目标地址的一批预建 TCP 连接。
// Get 优先拿现成的，池空时退回同步拨号 —— 所以延迟绝不会比不用池更差。
type connPool struct {
	addr string
	size int
	idle chan timedConn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newConnPool(addr string, size int) *connPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &connPool{
		addr:   addr,
		size:   size,
		idle:   make(chan timedConn, size),
		ctx:    ctx,
		cancel: cancel,
	}
	p.wg.Add(1)
	go p.replenish()
	return p
}

// Get 取一条可用连接。
func (p *connPool) Get() (net.Conn, error) {
	for {
		select {
		case tc := <-p.idle:
			if time.Since(tc.created) > poolConnMaxAge {
				tc.Close()
				continue
			}
			if isConnAlive(tc.Conn) {
				return tc.Conn, nil
			}
			tc.Close()
		default:
			return dialUpstream(p.addr)
		}
	}
}

// replenish 持续把池填到容量。
func (p *connPool) replenish() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		if len(p.idle) >= p.size {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(poolFillInterval):
			}
			continue
		}

		c, err := dialUpstream(p.addr)
		if err != nil {
			// 目标暂时连不上是常态（对端在重启）。退避后接着试，
			// 别把日志刷爆，也别放弃 —— 目标恢复后池要能自己填回来。
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(poolRetryBackoff):
			}
			continue
		}

		select {
		case p.idle <- timedConn{Conn: c, created: time.Now()}:
		case <-p.ctx.Done():
			c.Close()
			return
		}
	}
}

func (p *connPool) Close() {
	p.cancel()
	p.wg.Wait()
	close(p.idle)
	for tc := range p.idle {
		tc.Close()
	}
}

// isConnAlive 判断一条预建连接还能不能用。
//
// 关键在于用 MSG_PEEK：对端已经发过来的数据要原样留在内核缓冲里，
// 让真正的 relay 去读。普通 Read 会把第一个字节吃掉并丢弃，
// 那些「服务端先说话」的协议（SSH、SMTP、FTP 的欢迎横幅）就全坏了。
// MSG_DONTWAIT 保证探测不阻塞。
func isConnAlive(c net.Conn) bool {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return true // 不是 TCP（测试里的管道），探测不了，当作可用
	}
	rc, err := tcp.SyscallConn()
	if err != nil {
		return false
	}
	alive := false
	cerr := rc.Read(func(fd uintptr) bool {
		var buf [1]byte
		n, _, rerr := unix.Recvfrom(int(fd), buf[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case rerr == unix.EAGAIN || rerr == unix.EWOULDBLOCK:
			alive = true // 连接开着，只是暂时没数据
		case rerr != nil:
			alive = false // RST 或其他硬错误
		case n == 0:
			alive = false // 对端发了 FIN
		default:
			alive = true // 有数据在等，已经 peek 过没消费掉
		}
		return true // 永远返回 true：这是一次性探测，不等可读事件
	})
	if cerr != nil {
		return false
	}
	return alive
}
