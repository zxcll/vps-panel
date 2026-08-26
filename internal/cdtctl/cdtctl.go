// Package cdtctl 是「拿库里存的凭据去操作阿里云实例」这一层。
//
// 单独成包是为了解开一个依赖环：超额动作里新增了「CDT 节省关机」，
// 于是 internal/action 需要能开关阿里云实例；而构建客户端要读 cdt_accounts
// 并解密凭据，那段代码原本在 internal/server 里 —— action 反向依赖 server
// 会成环。
//
// 抽出来之后 server 和 action 都依赖它，谁也不依赖谁，顺带消掉了
// server.cdtClient 那份重复。
package cdtctl

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/store"
)

type Controller struct {
	st     *store.Store
	cipher *crypto.Cipher
	log    *slog.Logger
}

func New(st *store.Store, cipher *crypto.Cipher, log *slog.Logger) *Controller {
	return &Controller{st: st, cipher: cipher, log: log}
}

// ClientFor 用库里存的凭据构造一个阿里云客户端。
func (c *Controller) ClientFor(ctx context.Context, accountID int64) (*alicloud.Client, error) {
	a, err := c.st.GetCDTAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	enc, err := c.st.CDTAccountCred(ctx, accountID)
	if err != nil {
		return nil, err
	}
	secret, err := c.cipher.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("解密账号「%s」的凭据失败：%w（master.key 换过吗？）", a.Name, err)
	}
	if secret == "" {
		return nil, fmt.Errorf("账号「%s」还没有填写 AccessKeySecret", a.Name)
	}
	return alicloud.New(a.AccessKeyID, secret, a.RegionID, a.SiteType)
}

// StopInstance 停一台实例。mode 留空时用账号配的停机方式。
func (c *Controller) StopInstance(ctx context.Context, inst *store.CDTInstance, mode string) error {
	client, err := c.ClientFor(ctx, inst.AccountID)
	if err != nil {
		return err
	}
	if mode == "" {
		if a, err := c.st.GetCDTAccount(ctx, inst.AccountID); err == nil {
			mode = a.ShutdownMode
		}
	}
	if err := client.StopInstance(ctx, inst.InstanceID, mode); err != nil {
		return err
	}
	// 立刻把状态改成 Stopping，界面上马上能看到反应，不用等下一轮同步。
	return c.st.SetCDTInstanceStatus(ctx, inst.ID, alicloud.StatusStopping)
}

// StartInstance 开一台实例。
func (c *Controller) StartInstance(ctx context.Context, inst *store.CDTInstance) error {
	client, err := c.ClientFor(ctx, inst.AccountID)
	if err != nil {
		return err
	}
	if err := client.StartInstance(ctx, inst.InstanceID); err != nil {
		return err
	}
	return c.st.SetCDTInstanceStatus(ctx, inst.ID, alicloud.StatusStarting)
}

// InstanceOfNode 取节点关联的 CDT 实例。没关联时返回一个说得清楚的错误 ——
// 「超额动作设成了 CDT 关机但没关联实例」是个配置错误，得让人看得懂。
func (c *Controller) InstanceOfNode(ctx context.Context, n *store.Node) (*store.CDTInstance, error) {
	if n.CDTInstanceID == 0 {
		return nil, fmt.Errorf("节点「%s」没有关联阿里云 CDT 实例，"+
			"请先在节点编辑里选一个（超额动作设成「CDT 节省关机」时必须关联）", n.Name)
	}
	inst, err := c.st.GetCDTInstance(ctx, n.CDTInstanceID)
	if err != nil {
		return nil, fmt.Errorf("节点「%s」关联的 CDT 实例已不存在（可能被释放了），"+
			"请重新关联：%w", n.Name, err)
	}
	return inst, nil
}

// StopNodeInstance 通过关联的 CDT 实例把节点所在的机器停掉（节省关机）。
//
// 用在超额动作 cdt_stop 上。相比 SSH / 探针关机有两个好处：
// 停机之后不再收实例费；走的是阿里云 API，探针死了、SSH 连不上也照样能关。
func (c *Controller) StopNodeInstance(ctx context.Context, n *store.Node, reason string) (string, error) {
	inst, err := c.InstanceOfNode(ctx, n)
	if err != nil {
		return "", err
	}
	if err := c.StopInstance(ctx, inst, alicloud.StopModeCharging); err != nil {
		return "", fmt.Errorf("通过阿里云停机失败：%w", err)
	}
	if c.log != nil {
		c.log.Warn("已通过阿里云 CDT 停机", "节点", n.Name, "实例", inst.InstanceID, "原因", reason)
	}
	return fmt.Sprintf("已通过阿里云节省关机停掉实例 %s（停机后不再收实例费）", inst.InstanceID), nil
}

// NodeForInstance 反过来找：这台 CDT 实例被哪个节点关联着。
// 没有节点关联它时返回 nil, nil —— 那是常态，不是错误。
func (c *Controller) NodeForInstance(ctx context.Context, instanceID int64) (*store.Node, error) {
	nodes, err := c.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.CDTInstanceID == instanceID {
			return n, nil
		}
	}
	return nil, nil
}

// MarkNodePlannedStop 把关联节点标成「计划内停机」。
//
// 这是整个关联功能的意义所在：面板自己按计划停的机器，不该再被当成故障 ——
// 不发掉线告警，域名默认也不切走。
func (c *Controller) MarkNodePlannedStop(ctx context.Context, instanceID int64, reason string) {
	n, err := c.NodeForInstance(ctx, instanceID)
	if err != nil || n == nil {
		return
	}
	if err := c.st.SetNodeStatus(ctx, n.ID, store.StatusPlannedStop); err != nil {
		if c.log != nil {
			c.log.Warn("标记节点计划内停机失败", "节点", n.Name, "err", err)
		}
		return
	}
	id := n.ID
	c.st.AddEvent(ctx, &id, store.EventCDTAction, store.LevelInfo,
		fmt.Sprintf("节点「%s」进入计划内停机：%s（不会触发掉线告警，域名默认也不切）", n.Name, reason))
}

// ClearNodePlannedStop 把关联节点从「计划内停机」放出来。
//
// 只清 planned_stop 这一个状态，置回 unknown 让 engine 顺着心跳自然恢复成
// online —— 直接写 online 是不对的，机器刚下开机指令，探针还没连回来。
func (c *Controller) ClearNodePlannedStop(ctx context.Context, instanceID int64) {
	n, err := c.NodeForInstance(ctx, instanceID)
	if err != nil || n == nil || n.Status != store.StatusPlannedStop {
		return
	}
	if err := c.st.SetNodeStatus(ctx, n.ID, store.StatusUnknown); err != nil && c.log != nil {
		c.log.Warn("解除节点计划内停机失败", "节点", n.Name, "err", err)
	}
}
