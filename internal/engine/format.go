package engine

import (
	"fmt"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/store"
)

// humanBytes 把字节数格式化成人能读的形式。
// 用 1024 进制，和 VPS 商家标注流量的习惯一致。
func humanBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

// modeLabel 把计费口径转成中文说明。
func modeLabel(mode string) string {
	switch mode {
	case store.BillingMax:
		return "单向（出入站取大）"
	case store.BillingOut:
		return "仅出站"
	case store.BillingIn:
		return "仅入站"
	default:
		return "双向（出入站相加）"
	}
}

// actionLabel 把超额动作转成中文说明。
func actionLabel(a string) string {
	switch a {
	case store.ActionShutdownAgent:
		return "探针本地关机"
	case store.ActionShutdownSSH:
		return "SSH 远程关机"
	case store.ActionCommand:
		return "执行自定义命令"
	case store.ActionDNSOnly:
		return "仅切换解析"
	default:
		return "不处理"
	}
}

func channelLabel(via string) string {
	switch via {
	case action.ViaAgent:
		return "探针"
	case action.ViaSSH:
		return "SSH"
	default:
		return via
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
