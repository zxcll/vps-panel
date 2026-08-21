package forward

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

const (
	// relayBufSize 是单方向的拷贝缓冲。
	//
	// 之所以要包一层 reader 而不是直接 io.Copy：包过之后 io.CopyBuffer 会走
	// 通用的缓冲路径而不是 splice。splice 快，但字节数只有在连接关闭时才结算，
	// 而我们要的是连接跑着的时候就能持续看到计数增长。
	relayBufSize = 64 * 1024

	dialTimeout = 10 * time.Second

	// keepAlivePeriod 给两条腿都开 TCP keepalive。
	// 对端如果没发 FIN/RST 就消失了（拔网线、机器掉电），
	// 靠内核探测把 relay 的 goroutine 唤醒，否则它会一直挂着不释放。
	keepAlivePeriod = 30 * time.Second
)

// relayLinger 限制半关闭之后另一个方向还能跑多久。
//
// 卡在 FIN_WAIT2 的对端是不会被 keepalive 探测的（内核只对 ESTABLISHED 发探测包），
// 没有这个上限的话 goroutine 和它占的信号量名额就永久泄漏了。
// 时间给得比较宽松，因为「先半关闭、再慢慢吐一大段响应」是完全正常的用法。
//
// 写成 var 是为了测试能调小。
var relayLinger = 60 * time.Second

// setKeepAlive 给 TCP 连接开 keepalive；不是 TCP 的（测试里的管道）直接跳过。
func setKeepAlive(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(keepAlivePeriod)
	}
}

// dialUpstream 是打开上游连接的唯一入口，这样连接池里预建的和临时拨的
// 拿到的超时、keepalive 设置完全一致。
func dialUpstream(addr string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	setKeepAlive(c)
	return c, nil
}

// newLimiter 把 Mbps 换算成字节/秒的令牌桶，不限速时返回 nil。
// burst 至少要有一个缓冲那么大，否则单次 WaitN 会因为超过桶容量而永远等不到。
func newLimiter(mbps int) *rate.Limiter {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := float64(mbps) * 1e6 / 8.0
	burst := int(bytesPerSec)
	if burst < relayBufSize {
		burst = relayBufSize
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// meteredReader 在每次 Read 之后限速并计数。
// limPtr 可能指向 nil（不限速），counter 可能为 nil（这个方向不计数）。
type meteredReader struct {
	src     io.Reader
	limPtr  *atomic.Pointer[rate.Limiter]
	counter *atomic.Int64
	ctx     context.Context
}

func (r *meteredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		if r.limPtr != nil {
			if lim := r.limPtr.Load(); lim != nil {
				if werr := lim.WaitN(r.ctx, n); werr != nil {
					return n, werr
				}
			}
		}
		if r.counter != nil {
			r.counter.Add(int64(n))
		}
	}
	return n, err
}

// relayCopy 把 src 拷到 dst。限速器或计数器任一非空时包一层 meteredReader。
func relayCopy(ctx context.Context, dst io.Writer, src io.Reader, limPtr *atomic.Pointer[rate.Limiter], counter *atomic.Int64) {
	var r io.Reader = src
	if limPtr != nil || counter != nil {
		r = &meteredReader{src: src, limPtr: limPtr, counter: counter, ctx: ctx}
	}
	buf := make([]byte, relayBufSize)
	_, _ = io.CopyBuffer(dst, r, buf)
}

// halfCloseWrite 把单方向的 EOF 传下去，
// 让那些"关掉一半表示我说完了"的协议（HTTP/1.0、某些 RPC）能正常收尾。
func halfCloseWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
