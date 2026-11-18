package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestXiequIdentityCanonicalizesEquivalentPaths(t *testing.T) {
	base := XiequConfig{UID: "synthetic-account", BaseURL: "https://endpoint.invalid/api", Revision: "same-revision"}
	for _, address := range []string{"https://endpoint.invalid/%61pi", "https://ENDPOINT.invalid:443/a%70i", "https://endpoint.invalid/%61%70%69"} {
		alias := base
		alias.BaseURL = address
		if alias.accountIdentity() != base.accountIdentity() || alias.WhitelistScope() != base.WhitelistScope() {
			t.Fatalf("等价路径编码改变账号身份: %s", address)
		}
	}
}

// 合成旧版本存储：旧摘要保留 URL 的转义拼写，不含可逆账号材料。
func seedXiequLegacyJournal(t *testing.T, d *DB, c XiequConfig, oldURL, kind string, completed bool) (string, string) {
	t.Helper()
	digest := sha256.Sum256([]byte(c.UID + "\n" + oldURL))
	oldIdentity := hex.EncodeToString(digest[:])
	op := XiequOperation{Token: "original-legacy-token", Scope: oldIdentity + ":" + c.Revision, Kind: kind, IP: "203.0.113.99", Memo: "original-legacy-marker", Completed: completed}
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	key := "integration.xiequ.operation.v1:" + oldIdentity
	if _, err := d.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", key, string(payload)); err != nil {
		t.Fatal(err)
	}
	return key, op.Scope
}

func TestXiequLegacyEscapedJournalSurvivesUpgradeAndIdentityChanges(t *testing.T) {
	for _, kind := range []string{"add", "del"} {
		t.Run(kind, func(t *testing.T) {
			d, _ := fofaSettingsTestDB(t)
			uid, secret, address := "account-a", "synthetic-key", "https://endpoint.invalid/%61pi"
			c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &secret, BaseURL: &address})
			if err != nil {
				t.Fatal(err)
			}
			legacyKey, oldScope := seedXiequLegacyJournal(t, d, c, address, kind, false)
			state := XiequWhitelistState{Scope: oldScope, ManagedRecords: []XiequManagedRecord{{IP: "203.0.113.99", Memo: "original-legacy-marker", Scope: oldScope}}}
			if kind == "add" {
				state.PendingAdd = &XiequPendingAdd{IP: "203.0.113.99", Memo: "original-legacy-marker"}
			}
			payload, _ := json.Marshal(state)
			if _, err := d.Exec("UPDATE settings SET value=$1 WHERE key=$2", string(payload), settingXiequWhitelistState); err != nil {
				t.Fatal(err)
			}
			before, _, _ := d.GetSetting(legacyKey)
			observed, err := d.XiequWhitelistState()
			if err != nil || observed.Scope != c.WhitelistScope() || observed.LastStatus != "operation_unknown" || len(observed.ManagedRecords) != 1 || observed.ManagedRecords[0].Scope != c.WhitelistScope() {
				t.Fatalf("升级读取丢失归属/屏障: %+v %v", observed, err)
			}
			after, _, _ := d.GetSetting(legacyKey)
			if before != after {
				t.Fatal("只读状态修改了旧 journal")
			}
			if err := d.PreserveXiequPending(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			alias := "https://endpoint.invalid/api"
			current, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{BaseURL: &alias})
			if err != nil {
				t.Fatal(err)
			}
			if current.WhitelistScope() != c.WhitelistScope() {
				t.Fatal("等价配置切换不应换代或丢失归属")
			}
			for _, patch := range []XiequConfigPatch{{UKEY: stringPointer("rotated-key")}, {UID: stringPointer("account-b")}, {UID: &uid}} {
				current, err = d.SetXiequConfig(t.Context(), patch)
				if err != nil {
					t.Fatal(err)
				}
				op, err := d.XiequOperation(t.Context(), current)
				if err != nil {
					t.Fatal(err)
				}
				if current.UID == uid && (op == nil || op.Token != "original-legacy-token" || op.Kind != kind) {
					t.Fatalf("换密钥/ABA 绕过旧屏障: %+v", op)
				}
				if current.UID != uid && op != nil {
					t.Fatal("已有证明归属的 A 屏障污染 B")
				}
			}
			persisted, ok, err := d.GetSetting(legacyKey)
			var wire map[string]any
			if err != nil || !ok || json.Unmarshal([]byte(persisted), &wire) != nil || wire["completed"] == true || wire["token"] != "original-legacy-token" || wire["scope"] != oldScope || wire["account"] != c.accountIdentity() {
				t.Fatalf("旧 journal 被清除、完成或错误迁移: %s %v", persisted, err)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func TestXiequUnprovableLegacyAccountBlocksAllMutations(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, secret, address := "account-a", "synthetic-key", "https://endpoint.invalid/api"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &secret, BaseURL: &address})
	if err != nil {
		t.Fatal(err)
	}
	legacyKey, _ := seedXiequLegacyJournal(t, d, c, "https://endpoint.invalid/%61%70%69", "del", false)
	for _, patch := range []XiequConfigPatch{{}, {UID: stringPointer("account-b")}, {UID: &uid, UKEY: stringPointer("rotated-key")}} {
		current, err := d.SetXiequConfig(t.Context(), patch)
		if err != nil {
			t.Fatal(err)
		}
		if op, err := d.XiequOperation(t.Context(), current); err == nil {
			t.Fatalf("无法证明旧摘要归属却放行: %+v", op)
		}
		state := XiequWhitelistState{Scope: current.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.98", Memo: "new-marker", Prepared: true}}
		if err := d.SetXiequWhitelistState(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		state.Revision++
		op := XiequOperation{Token: "new-token", Scope: state.Scope, Kind: "add", IP: "203.0.113.98", Memo: "new-marker"}
		if err := d.ClaimXiequOperation(t.Context(), current, state, op); err == nil {
			t.Fatal("无法归属的旧 journal 未阻止新发送许可")
		}
	}
	raw, ok, err := d.GetSetting(legacyKey)
	if err != nil || !ok || !strings.Contains(raw, "original-legacy-token") || strings.Contains(raw, `"completed":true`) {
		t.Fatalf("旧未知 Delete 丢失: %s %v", raw, err)
	}
}

func TestXiequCompletedLegacyJournalDoesNotBlockNewAccount(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, secret := "account-a", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &secret})
	if err != nil {
		t.Fatal(err)
	}
	legacyKey, _ := seedXiequLegacyJournal(t, d, c, "https://unrelated.invalid/%61pi", "del", true)
	before, _, _ := d.GetSetting(legacyKey)
	if op, err := d.XiequOperation(t.Context(), c); err != nil || op != nil {
		t.Fatalf("有效已完成旧记录不应新建屏障: %+v %v", op, err)
	}
	state := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.98", Memo: "new-marker", Prepared: true}}
	if err := d.SetXiequWhitelistState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	state.Revision++
	op := XiequOperation{Token: "new-token", Scope: state.Scope, Kind: "add", IP: "203.0.113.98", Memo: "new-marker"}
	if err := d.ClaimXiequOperation(t.Context(), c, state, op); err != nil {
		t.Fatal(err)
	}
	after, _, _ := d.GetSetting(legacyKey)
	if before != after {
		t.Fatal("新操作改写了旧完成记录")
	}
}
