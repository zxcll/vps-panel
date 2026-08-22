package agent

import "time"

// 重连策略。
//
// 单独拎出来是因为它踩过坑，而且坑在 Run 那个大循环里很难看出来：
// backoff 变量声明在循环外、每轮翻倍、封顶 60 秒，**成功之后从不归位**。
// 于是跑了几天的探针只要断过几次，就永久停在 60 秒退避 —— 面板重启一下，
// 它要愣等一分钟才肯回来。
//
// 而面板那边「能不能给这台机器下发指令」看的正是这条长连接在不在：
// 下发转发规则、跑链路测试、推送探针升级，全都要它。长连接迟迟不回来，
// 界面上就会出现「节点管理里是在线的，转发页却显示离线、升级按钮点不动」
// 这种自相矛盾的样子。
//
// 做成一个小结构体，逻辑就能单独测了。

// 重连相关的几个参数。
const (
	// minBackoff 是重连的起步等待。面板重启通常只有几秒，起步短一点，
	// 长连接才能尽快恢复。
	minBackoff = time.Second
	// maxBackoff 是退避上限。
	maxBackoff = 30 * time.Second
	// stableConnection 是「这条连接算站住了」的时长。超过它就把退避归位。
	stableConnection = 30 * time.Second
	// wsFailuresBeforeHTTP 是连着失败多少次才降级 HTTP。
	//
	// 不能失败一次就降级：面板重启期间必然失败一两次，一降级就是好几分钟
	// 收不到指令，界面上这台机器会一直显示成没有长连接。
	wsFailuresBeforeHTTP = 4
	// httpFallbackWindow 是降级 HTTP 之后多久再试一次 WebSocket。
	httpFallbackWindow = 90 * time.Second
)

// reconnect 记着重连的退避时长和 WebSocket 的连续失败次数。
type reconnect struct {
	backoff    time.Duration
	wsFailures int
}

func newReconnect() *reconnect {
	return &reconnect{backoff: minBackoff}
}

// wait 返回这次该等多久，并把退避翻倍（封顶）。
func (r *reconnect) wait() time.Duration {
	d := r.backoff
	r.backoff *= 2
	if r.backoff > maxBackoff {
		r.backoff = maxBackoff
	}
	return d
}

// connectionEnded 在一次连接结束后调用，dur 是这条连接活了多久。
//
// 活够 stableConnection 就说明这条路本来是通的，只是断了一下 ——
// 退避和失败计数都归位，下次立刻重连。
func (r *reconnect) connectionEnded(dur time.Duration) {
	if dur >= stableConnection {
		r.backoff = minBackoff
		r.wsFailures = 0
	}
}

// wsFailed 记一次 WebSocket 连接失败，返回是否该降级成 HTTP 上报。
//
// 只有连着失败到一定次数才降级：面板重启期间必然失败一两次，
// 为这个就切到 HTTP 待上几分钟，代价比等一会儿大得多。
func (r *reconnect) wsFailed() bool {
	r.wsFailures++
	return r.wsFailures >= wsFailuresBeforeHTTP
}
