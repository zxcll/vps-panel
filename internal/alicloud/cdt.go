package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
)

// TrafficDetail 是一个业务地域上的 CDT 公网流量。
//
// 注意 Traffic 的单位是**字节**，而且只统计**出方向** —— CDT 的免费额度
// 本来就只算出方向，入方向不计费也不消耗额度。
type TrafficDetail struct {
	// BusinessRegionID 是业务地域，比如 cn-hangzhou、ap-southeast-1。
	// 免费额度分「中国内地」和「非中国内地」两个池，靠它来分。
	BusinessRegionID string `json:"business_region_id"`
	// TrafficType 是线路类型（BGP / BGP 精品之类），展示用。
	TrafficType string `json:"traffic_type"`
	Traffic     int64  `json:"traffic"`
}

// ListInternetTraffic 拉当前账号下所有地域的 CDT 公网流量。
//
// 这个接口返回的是**本自然月至今**的累计值，账号级、不分实例 ——
// 所以面板这边不需要自己做差值累加，直接存快照即可。
// 它和探针的网卡计数完全是两套口径，绝不能混进节点账本。
func (c *Client) ListInternetTraffic(ctx context.Context) ([]TrafficDetail, error) {
	var res struct {
		TrafficDetails []struct {
			BusinessRegionID string          `json:"BusinessRegionId"`
			TrafficType      string          `json:"TrafficType"`
			BusinessType     string          `json:"BusinessAccessType"`
			Traffic          json.RawMessage `json:"Traffic"`
		} `json:"TrafficDetails"`
	}
	if err := c.call(ctx, c.cdtEndpoint, "ListCdtInternetTraffic", versionCDT, nil, &res); err != nil {
		return nil, err
	}

	out := make([]TrafficDetail, 0, len(res.TrafficDetails))
	for _, d := range res.TrafficDetails {
		traffic, err := flexibleInt64(d.Traffic)
		if err != nil {
			return nil, fmt.Errorf("解析地域 %s 的流量值: %w", d.BusinessRegionID, err)
		}
		kind := d.TrafficType
		if kind == "" {
			kind = d.BusinessType
		}
		out = append(out, TrafficDetail{
			BusinessRegionID: d.BusinessRegionID,
			TrafficType:      kind,
			Traffic:          traffic,
		})
	}
	return out, nil
}
