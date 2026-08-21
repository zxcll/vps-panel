// Package action 负责对节点执行动作：关机、跑自定义命令。
//
// 两条通道：
//   - 探针通道：面板通过已建立的 WebSocket 下发指令。快，但探针挂了就没用
//   - SSH 通道：面板直连节点的 SSH。慢一点，但探针死了、机器还活着时唯一能用的手段
//
// 流量跑满要关机这种场景，SSH 通道是兜底，所以必须可靠。
package action

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/store"
)

// SSHRunner 通过 SSH 在节点上执行命令。
type SSHRunner struct {
	cipher *crypto.Cipher
	// DialTimeout 是建连超时。故障处置路径上不能卡太久。
	DialTimeout time.Duration
	// ExecTimeout 是命令执行超时。
	ExecTimeout time.Duration
}

func NewSSHRunner(c *crypto.Cipher) *SSHRunner {
	return &SSHRunner{
		cipher:      c,
		DialTimeout: 15 * time.Second,
		ExecTimeout: 30 * time.Second,
	}
}

// hostKeyMismatchError 单独定义，方便上层识别并升级成 error 级事件——
// 主机密钥变了要么是重装系统，要么是中间人，两种都得让用户知道。
type hostKeyMismatchError struct {
	expected, actual string
}

func (e *hostKeyMismatchError) Error() string {
	return fmt.Sprintf("SSH 主机密钥与记录不符，已拒绝连接。\n面板记录: %s\n本次收到: %s\n"+
		"如果这台机器刚重装过系统，请在节点设置里清空「主机密钥」后重试；否则可能存在中间人攻击",
		e.expected, e.actual)
}

// IsHostKeyMismatch 判断错误是否为主机密钥不匹配。
// x/crypto/ssh 会把回调返回的错误包一层，所以这里用 errors.As 而不是类型断言。
func IsHostKeyMismatch(err error) bool {
	var target *hostKeyMismatchError
	if errors.As(err, &target) {
		return true
	}
	// 握手失败的错误信息里也可能只剩文本形式
	return err != nil && strings.Contains(err.Error(), "SSH 主机密钥与记录不符")
}

// dialResult 带回本次连接看到的主机密钥，供 TOFU 首次连接时落库。
type dialResult struct {
	client  *ssh.Client
	hostKey string
}

func (r *SSHRunner) authMethods(n *store.Node) ([]ssh.AuthMethod, error) {
	secret, err := r.cipher.Decrypt(n.SSHSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("解密 SSH 凭据: %w", err)
	}
	if secret == "" {
		return nil, fmt.Errorf("节点未配置 SSH 密码或私钥")
	}

	if n.SSHAuth == "key" {
		passphrase, err := r.cipher.Decrypt(n.SSHKeyPassEnc)
		if err != nil {
			return nil, fmt.Errorf("解密私钥口令: %w", err)
		}
		var signer ssh.Signer
		if passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(secret), []byte(passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(secret))
		}
		if err != nil {
			return nil, fmt.Errorf("解析 SSH 私钥（若私钥有口令，请在「私钥口令」里填写）: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	// 密码认证。有些机器只开了 keyboard-interactive，这里两种都提供。
	return []ssh.AuthMethod{
		ssh.Password(secret),
		ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = secret
			}
			return answers, nil
		}),
	}, nil
}

func (r *SSHRunner) dial(ctx context.Context, n *store.Node) (*dialResult, error) {
	if n.SSHHost == "" {
		return nil, fmt.Errorf("节点未配置 SSH 地址")
	}

	auth, err := r.authMethods(n)
	if err != nil {
		return nil, err
	}

	port := n.SSHPort
	if port <= 0 {
		port = 22
	}
	addr := net.JoinHostPort(n.SSHHost, strconv.Itoa(port))

	res := &dialResult{}
	expected := strings.TrimSpace(n.SSHHostKey)

	cfg := &ssh.ClientConfig{
		User:    n.SSHUser,
		Auth:    auth,
		Timeout: r.DialTimeout,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			actual := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			res.hostKey = actual
			if expected == "" {
				// TOFU：首次连接信任并记录，之后严格比对
				return nil
			}
			if expected != actual {
				return &hostKeyMismatchError{expected: expected, actual: actual}
			}
			return nil
		},
	}

	d := net.Dialer{Timeout: r.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接 %s: %w", addr, err)
	}

	// 让 context 取消能打断 SSH 握手
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		// 带上 res：主机密钥回调可能已经拿到了对端的公钥，
		// 首次连接时上层要用它落库，密钥不符时要展示给用户看。
		return res, fmt.Errorf("SSH 握手失败: %w", err)
	}

	res.client = ssh.NewClient(c, chans, reqs)
	return res, nil
}

// Run 在节点上执行一条命令，返回合并后的 stdout+stderr。
// 同时返回本次连接看到的主机密钥，调用方在首次连接后应把它存起来。
func (r *SSHRunner) Run(ctx context.Context, n *store.Node, cmd string) (output, hostKey string, err error) {
	res, err := r.dial(ctx, n)
	if err != nil {
		return "", res.hostKeyOrEmpty(), err
	}
	defer res.client.Close()

	sess, err := res.client.NewSession()
	if err != nil {
		return "", res.hostKey, fmt.Errorf("创建 SSH 会话: %w", err)
	}
	defer sess.Close()

	type outcome struct {
		out []byte
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		b, e := sess.CombinedOutput(cmd)
		ch <- outcome{b, e}
	}()

	timeout := r.ExecTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	select {
	case o := <-ch:
		return string(o.out), res.hostKey, o.err
	case <-time.After(timeout):
		sess.Signal(ssh.SIGKILL) //nolint:errcheck
		return "", res.hostKey, fmt.Errorf("命令执行超过 %s 未返回", timeout)
	case <-ctx.Done():
		return "", res.hostKey, ctx.Err()
	}
}

func (d *dialResult) hostKeyOrEmpty() string {
	if d == nil {
		return ""
	}
	return d.hostKey
}

// Probe 只建连不执行命令，用于「测试 SSH 连接」按钮。
// 返回主机密钥，前端可以展示给用户确认。
func (r *SSHRunner) Probe(ctx context.Context, n *store.Node) (hostKey string, err error) {
	res, err := r.dial(ctx, n)
	if err != nil {
		return res.hostKeyOrEmpty(), err
	}
	defer res.client.Close()

	// 跑个无害的命令确认真的能执行，光握手成功不代表 shell 可用
	sess, err := res.client.NewSession()
	if err != nil {
		return res.hostKey, fmt.Errorf("创建 SSH 会话: %w", err)
	}
	defer sess.Close()
	if err := sess.Run("true"); err != nil {
		return res.hostKey, fmt.Errorf("执行测试命令失败: %w", err)
	}
	return res.hostKey, nil
}

// ShutdownScript 是关机命令。
//
// 两个讲究：
//  1. 用 nohup + & 把关机放到后台并延迟 2 秒，让 SSH 命令能正常返回。
//     否则机器立刻断电，SSH 连接被掐断，我们分不清"关机成功"和"命令没跑起来"。
//  2. 串联多个关机命令，不同发行版/精简镜像可用的不一样。
const ShutdownScript = `nohup sh -c 'sleep 2; shutdown -h now || poweroff || systemctl poweroff || halt -p' >/dev/null 2>&1 &`

// wrapSudo 在需要时加上 sudo 前缀。-n 表示不交互，避免卡在密码提示上。
func wrapSudo(cmd string, useSudo bool) string {
	if !useSudo {
		return cmd
	}
	return "sudo -n sh -c " + shellQuote(cmd)
}

// shellQuote 用单引号包裹字符串，内部单引号按 shell 规则转义。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Shutdown 通过 SSH 关闭节点。
func (r *SSHRunner) Shutdown(ctx context.Context, n *store.Node) (output, hostKey string, err error) {
	return r.Run(ctx, n, wrapSudo(ShutdownScript, n.SSHUseSudo))
}

// Exec 通过 SSH 执行用户自定义命令（如停止代理服务）。
func (r *SSHRunner) Exec(ctx context.Context, n *store.Node, script string) (output, hostKey string, err error) {
	if strings.TrimSpace(script) == "" {
		return "", "", fmt.Errorf("自定义命令为空")
	}
	return r.Run(ctx, n, wrapSudo(script, n.SSHUseSudo))
}
