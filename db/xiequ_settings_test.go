package db

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestXiequConfigEncryptedStorageAndDomains(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, ukey, enabled := "synthetic-uid", "synthetic-xiequ-key-never-echo", true
	domains := []string{"*.xiequ.cn", "proxy.example.test", "xiequ.cn"}
	config, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{
		Enabled: &enabled, UID: &uid, UKEY: &ukey, ProxyDomains: &domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.UID != uid || config.UKEY != ukey {
		t.Fatalf("配置保存结果不正确: %+v", config)
	}
	if len(config.ProxyDomains) != 2 || config.ProxyDomains[0] != "proxy.example.test" || config.ProxyDomains[1] != "xiequ.cn" {
		t.Fatalf("代理域名未规范化: %#v", config.ProxyDomains)
	}
	raw, ok, err := d.GetSetting(settingXiequ)
	if err != nil || !ok || strings.Contains(raw, ukey) {
		t.Fatal("UKEY 未加密保存")
	}
	encoded, err := json.Marshal(config)
	if err != nil || strings.Contains(string(encoded), ukey) || strings.Contains(string(encoded), uid) {
		t.Fatal("通用 JSON 不应包含 UID/UKEY")
	}
	reloaded, err := d.XiequConfig()
	if err != nil || reloaded.UID != uid || reloaded.UKEY != ukey || !reloaded.Enabled {
		t.Fatalf("配置恢复失败: %+v, %v", reloaded, err)
	}
}

func TestXiequConfigPatchPreservesSecret(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, ukey, enabled := "synthetic-uid", "synthetic-preserved-key", true
	if _, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &ukey, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	interval := 600
	config, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{Enabled: &disabled, RefreshIntervalSeconds: &interval})
	if err != nil || config.Enabled || config.UKEY != ukey || config.RefreshIntervalSeconds != interval {
		t.Fatalf("局部更新覆盖了 UKEY 或其它字段: %+v, %v", config, err)
	}
	empty := ""
	config, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{UKEY: &empty})
	if err != nil || config.UKEY != "" || config.Enabled {
		t.Fatalf("清除 UKEY 后未关闭功能: %+v, %v", config, err)
	}
}

func TestXiequProxyDomainMatchesHostAndSubdomainOnly(t *testing.T) {
	for _, host := range []string{"xiequ.cn", "api.xiequ.cn", "deep.api.xiequ.cn"} {
		if !XiequProxyDomainMatches(host, []string{"xiequ.cn"}) {
			t.Fatalf("%s 应匹配默认谐趣域名", host)
		}
	}
	for _, host := range []string{"notxiequ.cn", "xiequ.cn.example.com", "127.0.0.1"} {
		if XiequProxyDomainMatches(host, []string{"xiequ.cn"}) {
			t.Fatalf("%s 不应匹配谐趣域名", host)
		}
	}
	u, _ := url.Parse("socks5://user:pass@proxy.xiequ.cn:1080")
	if !XiequProxyDomainMatches(u.Hostname(), []string{"xiequ.cn"}) {
		t.Fatal("URL 主机名匹配失败")
	}
}

func TestXiequWhitelistStateNormalizesManagedIPs(t *testing.T) {
	state := normalizeXiequState(XiequWhitelistState{
		ManagedIPs: []string{"203.0.113.8", "203.0.113.7", "203.0.113.8", "bad"},
		LastIP:     "203.0.113.9",
	})
	if len(state.ManagedIPs) != 2 || state.ManagedIPs[0] != "203.0.113.7" || state.ManagedIPs[1] != "203.0.113.8" {
		t.Fatalf("白名单状态未去重排序: %#v", state.ManagedIPs)
	}
}
