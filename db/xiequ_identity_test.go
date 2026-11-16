package db

import (
	"errors"
	"testing"
)

func TestXiequAccountChangeClearsManagedIPs(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	first, second, key := "first-account", "second-account", "synthetic-key"
	_, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &first, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ExecContext(t.Context(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", settingXiequWhitelistState, `{"managed_ips":["203.0.113.8"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &second}); err != nil {
		t.Fatal(err)
	}
	state, err := d.XiequWhitelistState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ManagedIPs) != 0 {
		t.Fatalf("新账号继承了旧账号管理记录: %v", state.ManagedIPs)
	}
}

func TestXiequRejectsRefreshAcrossIdentityChangeAndReturn(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	first, second, key := "first-account", "second-account", "synthetic-key"
	old, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &first, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	oldState := XiequWhitelistState{Scope: old.WhitelistScope(), ManagedIPs: []string{"203.0.113.8"}}
	if err = d.SetXiequWhitelistState(t.Context(), oldState); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{second, first} {
		current, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid})
		if err != nil {
			t.Fatal(err)
		}
		if err = d.SetXiequWhitelistState(t.Context(), oldState); !errors.Is(err, ErrXiequConfigChanged) {
			t.Fatalf("旧刷新写回=%v", err)
		}
		state, err := d.XiequWhitelistState()
		if err != nil || len(state.ManagedIPs) != 0 || state.Scope != current.WhitelistScope() {
			t.Fatalf("状态被污染: %+v, %v", state, err)
		}
	}
}

func TestXiequIdentityNormalizationAndLegacyState(t *testing.T) {
	config := XiequConfig{UID: " account ", BaseURL: "HTTPS://EXAMPLE.TEST:443/api", Revision: "version"}
	equivalent := XiequConfig{UID: "account", BaseURL: "https://example.test/api", Revision: "version"}
	if config.WhitelistScope() != equivalent.WhitelistScope() {
		t.Fatal("等价账号身份未规范化")
	}
	legacy := XiequWhitelistState{ManagedIPs: []string{"203.0.113.8"}}
	if len(legacy.ForConfig(config).ManagedIPs) != 0 {
		t.Fatal("无身份旧记录不得认领")
	}
	equivalent.BaseURL = "https://other.test/api"
	if equivalent.WhitelistScope() == config.WhitelistScope() {
		t.Fatal("不同服务地址共享了身份")
	}
}
