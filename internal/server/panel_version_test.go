package server

import "testing"

func TestPanelReleaseNewerNeverOffersDowngrade(t *testing.T) {
	tests := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{name: "补丁版本升级", current: "1.6.1", latest: "1.6.2", want: true},
		{name: "两位补丁版本升级", current: "1.6.9", latest: "1.6.10", want: true},
		{name: "次版本升级", current: "1.6.9", latest: "1.7.0", want: true},
		{name: "版本相同", current: "1.6.2", latest: "1.6.2"},
		{name: "禁止降级", current: "1.6.2", latest: "1.6.1"},
		{name: "支持 v 前缀", current: "v1.6.1", latest: "v1.6.2", want: true},
		{name: "忽略构建后缀", current: "1.6.1+local", latest: "1.6.2", want: true},
		{name: "非法发布版本宁可不动", current: "1.6.1", latest: "latest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := panelReleaseNewer(tc.current, tc.latest); got != tc.want {
				t.Errorf("panelReleaseNewer(%q, %q) = %v，期望 %v",
					tc.current, tc.latest, got, tc.want)
			}
		})
	}
}
