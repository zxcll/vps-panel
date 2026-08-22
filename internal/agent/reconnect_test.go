package agent

import (
	"testing"
	"time"
)

// 这一组守的是同一件事：**连上过之后，退避必须归位**。
//
// 原来的写法把 backoff 声明在 Run 的大循环外面，每轮翻倍、封顶 60 秒，
// 成功之后从不重置。跑了几天的探针只要断过几次，就永久停在 60 秒 ——
// 面板重启一下，它要愣等一分钟才肯回来。而面板那边「能不能下发指令」
// 看的正是这条长连接在不在，于是界面上会出现：
// 节点管理里是在线的（心跳还在），转发页却显示离线、升级按钮点不动。

func TestReconnectBackoffGrows(t *testing.T) {
	r := newReconnect()

	if got := r.wait(); got != minBackoff {
		t.Errorf("起步应是 %v，实际 %v", minBackoff, got)
	}
	if got := r.wait(); got != 2*minBackoff {
		t.Errorf("第二次应翻倍到 %v，实际 %v", 2*minBackoff, got)
	}
	if got := r.wait(); got != 4*minBackoff {
		t.Errorf("第三次应是 %v，实际 %v", 4*minBackoff, got)
	}
}

func TestReconnectBackoffCaps(t *testing.T) {
	r := newReconnect()
	for range 20 {
		r.wait()
	}
	if got := r.wait(); got != maxBackoff {
		t.Errorf("退避应封顶在 %v，实际 %v", maxBackoff, got)
	}
}

// 核心用例：连接站住过之后，退避必须回到起点。
func TestReconnectResetsAfterStableConnection(t *testing.T) {
	r := newReconnect()

	// 先失败几轮，把退避顶到上限。
	for range 10 {
		r.wait()
	}
	if r.backoff != maxBackoff {
		t.Fatalf("准备阶段没顶到上限：%v", r.backoff)
	}

	// 然后连上了，而且撑了足够久。
	r.connectionEnded(stableConnection)

	if got := r.wait(); got != minBackoff {
		t.Errorf("连接站住过之后应回到 %v，实际 %v —— "+
			"不归位的话面板重启后探针要愣等一分钟才回来", minBackoff, got)
	}
}

// 连上就断（比如面板正在重启、握手完就被踢），不算站住，退避继续涨。
func TestReconnectKeepsBackoffAfterFlappingConnection(t *testing.T) {
	r := newReconnect()
	r.wait() // 1s → 2s

	r.connectionEnded(time.Millisecond)

	if got := r.wait(); got == minBackoff {
		t.Error("连上立刻就断不该算站住，退避不该归位")
	}
}

// WebSocket 失败一两次不该降级 HTTP —— 面板重启期间必然失败，
// 一降级就是好几分钟收不到指令。
func TestReconnectDoesNotFallBackTooEagerly(t *testing.T) {
	r := newReconnect()

	for i := 1; i < wsFailuresBeforeHTTP; i++ {
		if r.wsFailed() {
			t.Fatalf("第 %d 次失败就降级了，太急躁 —— 面板重启期间必然失败一两次", i)
		}
	}
	if !r.wsFailed() {
		t.Errorf("连着失败 %d 次之后应该降级 HTTP 了", wsFailuresBeforeHTTP)
	}
}

// 中间只要成功站住过一次，失败计数就该清零，重新开始数。
func TestReconnectFailureCountResetsOnSuccess(t *testing.T) {
	r := newReconnect()

	r.wsFailed()
	r.wsFailed()
	r.connectionEnded(stableConnection)

	// 清零之后，又要重新数满 wsFailuresBeforeHTTP 次才降级。
	for i := 1; i < wsFailuresBeforeHTTP; i++ {
		if r.wsFailed() {
			t.Fatalf("失败计数没清零：站住过之后第 %d 次失败就降级了", i)
		}
	}
}

// 参数本身要说得过去：起步别太长，上限别太长。
// 这条挡的是"随手把 minBackoff 改成 30 秒"这类回归。
func TestReconnectTimingsAreSane(t *testing.T) {
	if minBackoff > 2*time.Second {
		t.Errorf("起步退避 %v 太长了，面板重启后长连接要等太久才恢复", minBackoff)
	}
	if maxBackoff > time.Minute {
		t.Errorf("退避上限 %v 太长了", maxBackoff)
	}
	if httpFallbackWindow > 3*time.Minute {
		t.Errorf("降级 HTTP 的时长 %v 太长了 —— 这期间面板发不出任何指令",
			httpFallbackWindow)
	}
}
