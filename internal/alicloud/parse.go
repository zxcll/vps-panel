package alicloud

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 阿里云这几个老接口对数值字段的表达很不统一：同一个字段有时给 12345、
// 有时给 "12345"，账单接口还会给带千分位逗号的 "1,234.56"，流量接口偶尔
// 给科学计数法。硬按 int64/float64 解会在某些账号上直接失败 ——
// 而失败的后果是流量或账单读不出来，熔断判定拿不到数据，所以这里一律宽容处理。

// flexibleFloat 把一个可能是数字、也可能是字符串的 JSON 值解成 float64。
func flexibleFloat(raw json.RawMessage) (float64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}

	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, nil
	}

	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, fmt.Errorf("既不是数字也不是字符串：%s", s)
	}
	// 千分位逗号和货币符号都得去掉，"1,234.56" 直接 ParseFloat 会失败。
	str = strings.TrimSpace(strings.NewReplacer(",", "", "¥", "", "$", "", " ", "").Replace(str))
	if str == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, fmt.Errorf("字符串 %q 解析不出数字", string(raw))
	}
	return v, nil
}

// flexibleInt64 同上，结果取整。流量是字节数，用不着小数。
func flexibleInt64(raw json.RawMessage) (int64, error) {
	f, err := flexibleFloat(raw)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}
