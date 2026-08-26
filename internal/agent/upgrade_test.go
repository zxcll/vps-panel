package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/zxcll/vps-panel/internal/protocol"
)

// 替换二进制那几步危险操作的用例在 internal/selfupdate 里 ——
// 面板和探针共用同一份实现，测试也就跟着放在那儿，不在这边再抄一遍。
// 这里只测探针特有的部分。

// 关掉 --allow-upgrade 之后，面板推升级必须被拒。
func TestUpgradeRefusedWhenDisabled(t *testing.T) {
	a := &Agent{cfg: Config{AllowUpgrade: false}, log: quietLogger()}

	res := a.upgrade(context.Background(), nil)
	if res.OK {
		t.Fatal("禁用远程升级后不该执行")
	}
	if !strings.Contains(res.Error, "allow-upgrade") {
		t.Errorf("错误消息应指出是哪个开关关着，实际：%s", res.Error)
	}
}

// 已经是目标版本就别白折腾一次重启 —— 面板批量升级时，
// 大部分节点本来就已经是最新的。
func TestUpgradeSkipsWhenAlreadyOnTarget(t *testing.T) {
	a := &Agent{cfg: Config{AllowUpgrade: true}, log: quietLogger()}

	res := a.upgrade(context.Background(), &protocol.UpgradeRequest{Version: Version})
	if !res.OK {
		t.Fatalf("同版本应当直接返回成功：%s", res.Error)
	}
	if !strings.Contains(res.Output, "无需升级") {
		t.Errorf("应说明是无需升级，实际 %s", res.Output)
	}
}
