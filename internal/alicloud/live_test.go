package alicloud

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive 打真实的阿里云，确认响应字段名和我们解析的一致。
//
// 默认跳过：只有设置了 ALICLOUD_LIVE_AK / ALICLOUD_LIVE_SK 才跑。
// 这样 make test 和 CI 永远不会碰真实账号，也不会因为没网就红。
//
// **只调只读接口。** StartInstance / StopInstance 这类会真动机器的，
// 一律靠 httptest 验参数拼装，绝不在这里碰真实账号。
func TestLive(t *testing.T) {
	ak, sk := os.Getenv("ALICLOUD_LIVE_AK"), os.Getenv("ALICLOUD_LIVE_SK")
	if ak == "" || sk == "" {
		t.Skip("未设置 ALICLOUD_LIVE_AK / ALICLOUD_LIVE_SK，跳过真实账号测试")
	}

	site := os.Getenv("ALICLOUD_LIVE_SITE")
	if site == "" {
		site = SiteInternational
	}
	region := os.Getenv("ALICLOUD_LIVE_REGION")
	if region == "" {
		region = "ap-southeast-1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := New(ak, sk, region, site)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	t.Run("CDT流量", func(t *testing.T) {
		details, err := c.ListInternetTraffic(ctx)
		if err != nil {
			t.Fatalf("拉 CDT 流量失败: %v", err)
		}
		t.Logf("共 %d 条地域明细", len(details))
		for _, d := range details {
			t.Logf("  地域=%-16s 线路=%-10s 流量=%d 字节  → 归入「%s」池",
				d.BusinessRegionID, d.TrafficType, d.Traffic,
				classifyForLog(d.BusinessRegionID))
		}
	})

	t.Run("账号余额", func(t *testing.T) {
		bal, err := c.QueryAccountBalance(ctx)
		if err != nil {
			t.Fatalf("查余额失败: %v", err)
		}
		t.Logf("余额 = %s%.2f %s", bal.Symbol, bal.AvailableAmount, bal.Currency)
	})

	t.Run("账单概览", func(t *testing.T) {
		bill, err := c.QueryBillOverview(ctx, "")
		if err != nil {
			t.Fatalf("查账单失败: %v", err)
		}
		t.Logf("账期 %s 待还合计 = %s%.2f，共 %d 个产品条目",
			bill.BillingCycle, bill.Symbol, bill.TotalOutstanding, len(bill.Items))
		for _, item := range bill.Items {
			t.Logf("  %s：应付 %.2f 待还 %.2f", item.Product, item.PretaxAmount, item.OutstandingAmount)
		}
	})

	t.Run("ECS实例", func(t *testing.T) {
		insts, err := c.DescribeInstances(ctx)
		if err != nil {
			t.Fatalf("拉实例列表失败（地域 %s）: %v", region, err)
		}
		t.Logf("地域 %s 下共 %d 台实例", region, len(insts))
		for _, i := range insts {
			t.Logf("  %s（%s）状态=%s IP=%s 规格=%s 抢占式=%v 带宽=%dMbps",
				i.InstanceID, i.InstanceName, i.Status, i.PublicIP,
				i.InstanceType, i.IsSpot, i.BandwidthMbps)
		}
	})
}

// classifyForLog 只是为了让日志能直接看出分池对不对，
// 避免把 internal/cdt 依赖进来（那会形成 alicloud → cdt 的耦合）。
func classifyForLog(region string) string {
	switch region {
	case "cn-hongkong", "cn-macau", "cn-taipei":
		return "非中国内地"
	}
	if len(region) > 3 && region[:3] == "cn-" {
		return "中国内地"
	}
	return "非中国内地"
}
