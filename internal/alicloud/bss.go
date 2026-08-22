package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
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
