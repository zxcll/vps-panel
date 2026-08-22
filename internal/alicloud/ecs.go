package alicloud

import (
	"context"
	"fmt"
)

// 停机模式。
const (
	// StopModeCharging 是「节省模式」：停机后不再收实例费（按量付费的 VPC 实例），
	// 代价是公网 IP 可能会变。CDT 场景下这才是想要的 —— 用户停机就是为了省钱。
	StopModeCharging = "StopCharging"
	// StopModeKeepCharging 是普通停机：继续计费、保留资源。
	StopModeKeepCharging = "KeepCharging"
)

// 实例状态。阿里云原样返回这几个字符串。
const (
	StatusRunning  = "Running"
	StatusStopped  = "Stopped"
	StatusStarting = "Starting"
	StatusStopping = "Stopping"
)

// ErrCodeNoStock 是抢占式实例所在可用区售罄时返回的错误码。
//
// 保活逻辑必须能认出它：售罄是「等一会儿再试」，不是「配置错了」，
// 两者的处理方式完全不同（前者持续重试，后者应该告警让人去看）。
const ErrCodeNoStock = "NoStock"

// Instance 是一台 ECS 实例的快照。
type Instance struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	Status       string `json:"status"`
	PublicIP     string `json:"public_ip"`
	InstanceType string `json:"instance_type"`
	RegionID     string `json:"region_id"`
	ZoneID       string `json:"zone_id"`
	// IsSpot 表示这是抢占式（竞价）实例，可能被阿里云随时回收。
	// 保活功能只对它有意义。
	IsSpot        bool `json:"is_spot"`
	BandwidthMbps int  `json:"bandwidth_mbps"`
}

// DescribeInstances 列出这个地域下的全部实例。
//
// 翻页拉全：只拉第一页的话，实例多的账号会漏掉后面的，而漏掉的那台
// 恰好是被守护的实例时，熔断和保活都会静默失效。
func (c *Client) DescribeInstances(ctx context.Context) ([]Instance, error) {
	const pageSize = 100
	out := []Instance{}

	for page := 1; ; page++ {
		var res struct {
			TotalCount int `json:"TotalCount"`
			Instances  struct {
				Instance []struct {
					InstanceID       string `json:"InstanceId"`
					InstanceName     string `json:"InstanceName"`
					Status           string `json:"Status"`
					InstanceType     string `json:"InstanceType"`
					RegionID         string `json:"RegionId"`
					ZoneID           string `json:"ZoneId"`
					SpotStrategy     string `json:"SpotStrategy"`
					InternetMaxBWOut int    `json:"InternetMaxBandwidthOut"`
					PublicIPAddress  struct {
						IPAddress []string `json:"IpAddress"`
					} `json:"PublicIpAddress"`
					EipAddress struct {
						IPAddress string `json:"IpAddress"`
					} `json:"EipAddress"`
				} `json:"Instance"`
			} `json:"Instances"`
		}

		params := map[string]string{
			"RegionId":   c.region,
			"PageSize":   fmt.Sprintf("%d", pageSize),
			"PageNumber": fmt.Sprintf("%d", page),
		}
		if err := c.call(ctx, c.ecsEndpoint, "DescribeInstances", versionECS, params, &res); err != nil {
			return nil, err
		}

		for _, inst := range res.Instances.Instance {
			// EIP 优先：绑了弹性公网 IP 的话，那个才是对外真正在用的地址。
			ip := inst.EipAddress.IPAddress
			if ip == "" && len(inst.PublicIPAddress.IPAddress) > 0 {
				ip = inst.PublicIPAddress.IPAddress[0]
			}
			out = append(out, Instance{
				InstanceID:    inst.InstanceID,
				InstanceName:  inst.InstanceName,
				Status:        inst.Status,
				PublicIP:      ip,
				InstanceType:  inst.InstanceType,
				RegionID:      inst.RegionID,
				ZoneID:        inst.ZoneID,
				IsSpot:        inst.SpotStrategy != "" && inst.SpotStrategy != "NoSpot",
				BandwidthMbps: inst.InternetMaxBWOut,
			})
		}

		if len(res.Instances.Instance) < pageSize || len(out) >= res.TotalCount {
			return out, nil
		}
	}
}

// DescribeInstanceStatus 只查一台实例的状态。
// 保活循环每轮都要用它，比拉整个列表轻得多。
func (c *Client) DescribeInstanceStatus(ctx context.Context, instanceID string) (string, error) {
	var res struct {
		InstanceStatuses struct {
			InstanceStatus []struct {
				InstanceID string `json:"InstanceId"`
				Status     string `json:"Status"`
			} `json:"InstanceStatus"`
		} `json:"InstanceStatuses"`
	}
	params := map[string]string{
		"RegionId":     c.region,
		"InstanceId.1": instanceID,
	}
	if err := c.call(ctx, c.ecsEndpoint, "DescribeInstanceStatus", versionECS, params, &res); err != nil {
		return "", err
	}
	for _, s := range res.InstanceStatuses.InstanceStatus {
		if s.InstanceID == instanceID {
			return s.Status, nil
		}
	}
	return "", fmt.Errorf("实例 %s 不在返回结果里（可能已被释放）", instanceID)
}

// StartInstance 开机。
//
// 抢占式实例所在可用区售罄时会返回 NoStock，调用方用 IsCode(err, ErrCodeNoStock)
// 判断，别去匹配错误消息里的中文。
func (c *Client) StartInstance(ctx context.Context, instanceID string) error {
	return c.call(ctx, c.ecsEndpoint, "StartInstance", versionECS, map[string]string{
		"RegionId":   c.region,
		"InstanceId": instanceID,
	}, nil)
}

// StopInstance 关机。
//
// ForceStop 一律传 false：强制停机等同于直接拔电，未落盘的数据会丢。
// 这是自动化路径上的操作，宁可停不下来让人来看，也不能悄悄毁数据。
func (c *Client) StopInstance(ctx context.Context, instanceID, mode string) error {
	if mode != StopModeKeepCharging {
		mode = StopModeCharging
	}
	return c.call(ctx, c.ecsEndpoint, "StopInstance", versionECS, map[string]string{
		"RegionId":    c.region,
		"InstanceId":  instanceID,
		"StoppedMode": mode,
		"ForceStop":   "false",
	}, nil)
}
