package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Balance 是账号余额。
type Balance struct {
	AvailableAmount float64 `json:"available_amount"`
	Currency        string  `json:"currency"`
	Symbol          string  `json:"symbol"`
}

// BillOverview 是某个账期的账单概览。
type BillOverview struct {
	BillingCycle string `json:"billing_cycle"`
	// TotalOutstanding 是这个账期的待还总额。余额不足时阿里云会欠费停机，
	// 所以它是比余额更直接的风险信号。
	TotalOutstanding float64    `json:"total_outstanding"`
	Currency         string     `json:"currency"`
	Symbol           string     `json:"symbol"`
	Items            []BillItem `json:"items"`
	FetchedAt        time.Time  `json:"fetched_at"`
}

// BillItem 是账单里的一个产品条目。
type BillItem struct {
	Product           string  `json:"product"`
	PretaxAmount      float64 `json:"pretax_amount"`
	OutstandingAmount float64 `json:"outstanding_amount"`
}

// detectScheme 做成变量只为让单测能指向本地的 httptest 服务器。
var detectScheme = "https"

// DetectSite 认一下这组凭据属于中国站还是国际站。
//
// 为什么要自动认：账单接口在两个站点是不同域名，选错了余额和待还就一直拉不到，
// 而报错长得像「权限不够」，用户很难联想到是站点选错了。ECS 地域也推不出站点 ——
// 国际站账号照样可以在 cn-hangzhou 开机器。
//
// 做法很直接：拿两个域名各查一次余额，哪个通就是哪个。这是只读请求，
// 代价就是最多两次调用。
//
// 返回 site 和该站点的记账货币。两个都不通说明凭据本身有问题，
// 把两边的错误都带回去 —— 只报一个会让人以为是那个站点的问题。
func DetectSite(ctx context.Context, keyID, secret, region string) (site, currency string, err error) {
	var failures []string
	// 先试国际站：用这个面板的人绝大多数是国际站（CDT 免费额度那 200GB
	// 就是冲着非中国内地去的）。
	for _, candidate := range []string{SiteInternational, SiteChina} {
		c, buildErr := New(keyID, secret, region, candidate)
		if buildErr != nil {
			return "", "", buildErr
		}
		c.scheme = detectScheme
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, callErr := c.QueryAccountBalance(probeCtx)
		cancel()

		if callErr == nil {
			cur, _ := c.Currency()
			return candidate, cur, nil
		}
		failures = append(failures, fmt.Sprintf("%s（%s）：%v",
			siteLabel(candidate), bssEndpoints[candidate], callErr))
	}
	return "", "", fmt.Errorf("认不出这组凭据属于哪个站点，两边的账单接口都不通：\n  %s",
		strings.Join(failures, "\n  "))
}

func siteLabel(site string) string {
	if site == SiteChina {
		return "中国站"
	}
	return "国际站"
}

// QueryAccountBalance 查账号余额。
func (c *Client) QueryAccountBalance(ctx context.Context) (*Balance, error) {
	var res struct {
		Data struct {
			AvailableAmount json.RawMessage `json:"AvailableAmount"`
			Currency        string          `json:"Currency"`
		} `json:"Data"`
	}
	if err := c.call(ctx, c.bssEndpoint, "QueryAccountBalance", versionBSS, nil, &res); err != nil {
		return nil, err
	}

	// 余额是带千分位逗号的字符串（"1,234.56"），不能直接按数字解。
	amount, err := flexibleFloat(res.Data.AvailableAmount)
	if err != nil {
		return nil, fmt.Errorf("解析账号余额: %w", err)
	}

	currency, symbol := c.Currency()
	if res.Data.Currency != "" {
		currency = res.Data.Currency
	}
	return &Balance{AvailableAmount: amount, Currency: currency, Symbol: symbol}, nil
}

// QueryBillOverview 查某个账期的账单概览。cycle 传空即当前自然月（YYYY-MM）。
func (c *Client) QueryBillOverview(ctx context.Context, cycle string) (*BillOverview, error) {
	if cycle == "" {
		cycle = now().Format("2006-01")
	}

	var res struct {
		Data struct {
			BillingCycle string `json:"BillingCycle"`
			Items        struct {
				Item []struct {
					ProductName       string          `json:"ProductName"`
					ProductCode       string          `json:"ProductCode"`
					PretaxAmount      json.RawMessage `json:"PretaxAmount"`
					OutstandingAmount json.RawMessage `json:"OutstandingAmount"`
				} `json:"Item"`
			} `json:"Items"`
		} `json:"Data"`
	}
	if err := c.call(ctx, c.bssEndpoint, "QueryBillOverview", versionBSS,
		map[string]string{"BillingCycle": cycle}, &res); err != nil {
		return nil, err
	}

	currency, symbol := c.Currency()
	out := &BillOverview{
		BillingCycle: cycle,
		Currency:     currency,
		Symbol:       symbol,
		Items:        []BillItem{},
		FetchedAt:    now(),
	}
	if res.Data.BillingCycle != "" {
		out.BillingCycle = res.Data.BillingCycle
	}

	for _, item := range res.Data.Items.Item {
		outstanding, err := flexibleFloat(item.OutstandingAmount)
		if err != nil {
			return nil, fmt.Errorf("解析产品 %s 的待还金额: %w", item.ProductName, err)
		}
		pretax, err := flexibleFloat(item.PretaxAmount)
		if err != nil {
			return nil, fmt.Errorf("解析产品 %s 的应付金额: %w", item.ProductName, err)
		}
		out.TotalOutstanding += outstanding

		// 金额全是 0 的条目没有展示价值，滤掉免得把列表撑满。
		if pretax == 0 && outstanding == 0 {
			continue
		}
		name := item.ProductName
		if name == "" {
			name = item.ProductCode
		}
		out.Items = append(out.Items, BillItem{
			Product:           name,
			PretaxAmount:      pretax,
			OutstandingAmount: outstanding,
		})
	}
	return out, nil
}
