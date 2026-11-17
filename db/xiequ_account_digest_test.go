package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func seedXiequAccountDigestJournal(t *testing.T, d *DB, c XiequConfig, accountName, accountJSON string, completed bool) (string, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(c.UID + "\n" + c.BaseURL))
	keyHash := hex.EncodeToString(sum[:])
	key := "integration.xiequ.operation.v1:" + keyHash
	payload := fmt.Sprintf(`{"token":"original-token","scope":%q,"kind":"del","ip":"203.0.113.99","memo":"original-marker"`, keyHash+":"+c.Revision)
	if completed {
		payload += `,"completed":true`
	}
	if accountName != "" {
		payload += fmt.Sprintf(",%q:%s", accountName, accountJSON)
	}
	payload += `}`
	if _, err := d.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", key, payload); err != nil {
		t.Fatal(err)
	}
	return key, payload
}

func TestXiequLegacyAccountDigestMustBeCanonicalOnReadAndClaim(t *testing.T) {
	valid := "CURRENT"
	tests := []struct {
		name           string
		accountName    string
		account        string
		otherKey       bool
		completed      bool
		wantReadError  bool
		wantOperation  bool
		wantClaimError bool
	}{
		{name: "no-account-but-current-v1-hash-proves-identity", wantOperation: true, wantClaimError: true},
		{name: "valid-bound-current", account: `"` + valid + `"`, wantOperation: true, wantClaimError: true},
		{name: "valid-bound-other", account: `"OTHER"`, otherKey: true},
		{name: "conflicting-current-key-binding", account: `"OTHER"`, wantReadError: true, wantClaimError: true},
		{name: "uppercase-property", accountName: "Account", account: `"CURRENT"`, wantReadError: true, wantClaimError: true},
		{name: "invalid-characters", account: `"` + strings.Repeat("z", 64) + `"`, wantReadError: true, wantClaimError: true},
		{name: "short", account: `"` + strings.Repeat("a", 63) + `"`, wantReadError: true, wantClaimError: true},
		{name: "long", account: `"` + strings.Repeat("a", 65) + `"`, wantReadError: true, wantClaimError: true},
		{name: "uppercase", account: `"` + strings.Repeat("A", 64) + `"`, wantReadError: true, wantClaimError: true},
		{name: "number", account: `123`, wantReadError: true, wantClaimError: true},
		{name: "null", account: `null`, wantReadError: true, wantClaimError: true},
		{name: "array", account: `[]`, wantReadError: true, wantClaimError: true},
		{name: "object", account: `{}`, wantReadError: true, wantClaimError: true},
		{name: "boolean", account: `false`, wantReadError: true, wantClaimError: true},
		{name: "explicit-empty", account: `""`, wantReadError: true, wantClaimError: true},
		{name: "mixed-case", account: `"` + strings.Repeat("aB", 32) + `"`, wantReadError: true, wantClaimError: true},
		{name: "malformed-completed", account: `null`, completed: true, wantReadError: true, wantClaimError: true},
		{name: "nonhex-completed", account: `"` + strings.Repeat("z", 64) + `"`, completed: true, wantReadError: true, wantClaimError: true},
		{name: "valid-completed", account: `"` + valid + `"`, completed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := fofaSettingsTestDB(t)
			uid, secret := "account-a", "synthetic-key"
			c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &secret})
			if err != nil {
				t.Fatal(err)
			}
			accountJSON := tc.account
			journalConfig := c
			otherConfig := c
			otherConfig.UID = "account-b"
			if accountJSON == `"CURRENT"` {
				accountJSON = `"` + c.accountIdentity() + `"`
			} else if accountJSON == `"OTHER"` {
				accountJSON = `"` + otherConfig.accountIdentity() + `"`
			}
			if tc.otherKey {
				journalConfig = otherConfig
			}
			accountName := tc.accountName
			if accountJSON != "" && accountName == "" {
				accountName = "account"
			}
			key, original := seedXiequAccountDigestJournal(t, d, journalConfig, accountName, accountJSON, tc.completed)
			observed, readErr := d.XiequOperation(t.Context(), c)
			if (readErr != nil) != tc.wantReadError {
				t.Errorf("read error=%v wantReadError=%v", readErr, tc.wantReadError)
			}
			if (observed != nil) != tc.wantOperation {
				t.Errorf("operation=%+v wantOperation=%v readErr=%v", observed, tc.wantOperation, readErr)
			}
			state := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.98", Memo: "new-marker", Prepared: true}}
			if err := d.SetXiequWhitelistState(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			state.Revision++
			claimErr := d.ClaimXiequOperation(t.Context(), c, state, XiequOperation{
				Token: "new-token", Scope: state.Scope, Kind: "add", IP: "203.0.113.98", Memo: "new-marker",
			})
			if tc.wantClaimError && claimErr == nil {
				t.Error("坏摘要或未知操作未阻止发送许可")
			}
			if tc.wantReadError && (claimErr == nil || readErr == nil || claimErr.Error() != readErr.Error()) {
				t.Errorf("Claim 未以同一持久化损坏原因失败关闭: read=%v claim=%v", readErr, claimErr)
			}
			if tc.wantOperation && !errors.Is(claimErr, ErrXiequOperationPending) {
				t.Errorf("Claim 未被当前账号未知操作屏障阻止: %v", claimErr)
			}
			if !tc.wantClaimError && claimErr != nil {
				t.Errorf("有效无屏障记录错误阻止新操作: %v", claimErr)
			}
			after, ok, err := d.GetSetting(key)
			if err != nil || !ok || after != original {
				t.Fatalf("journal 被读写或准入路径改写: %q %v", after, err)
			}
		})
	}
}

func TestXiequMalformedV2AccountCannotBeReadOrClaimed(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, secret := "account-a", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &secret})
	if err != nil {
		t.Fatal(err)
	}
	state := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.98", Memo: "new-marker", Prepared: true}}
	if err := d.SetXiequWhitelistState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	state.Revision++
	sum := sha256.Sum256([]byte(c.UID + "\n" + c.BaseURL))
	key := "integration.xiequ.operation.v2:" + hex.EncodeToString(sum[:])
	for _, account := range []string{"", strings.Repeat("z", 64), strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("b", 64)} {
		for _, completed := range []bool{false, true} {
			op := XiequOperation{Token: "bad", Scope: c.WhitelistScope(), Kind: "del", IP: "203.0.113.99", Memo: "marker", Account: account, Completed: completed}
			payload, _ := json.Marshal(op)
			if _, err := d.Exec("INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", key, string(payload)); err != nil {
				t.Fatal(err)
			}
			_, readErr := d.XiequOperation(t.Context(), c)
			if readErr == nil {
				t.Fatalf("损坏或矛盾的 v2 account 被接受: %q completed=%v", account, completed)
			}
			op = XiequOperation{Token: "new", Scope: state.Scope, Kind: "add", IP: state.PendingAdd.IP, Memo: state.PendingAdd.Memo}
			claimErr := d.ClaimXiequOperation(t.Context(), c, state, op)
			if claimErr == nil || claimErr.Error() != readErr.Error() {
				t.Fatalf("Claim 未因同一 v2 account 错误失败关闭: read=%v claim=%v", readErr, claimErr)
			}
			// 升级恢复不能用 pending 覆盖损坏 completed v2 屏障。
			legacyPending := state
			legacyPending.PendingAdd = &XiequPendingAdd{IP: state.PendingAdd.IP, Memo: state.PendingAdd.Memo}
			if err := d.SetXiequWhitelistState(t.Context(), legacyPending); err != nil {
				t.Fatal(err)
			}
			preserveErr := d.PreserveXiequPending(t.Context(), c)
			if preserveErr == nil || preserveErr.Error() != readErr.Error() {
				t.Fatalf("恢复路径覆盖或忽略损坏 v2 account: read=%v preserve=%v", readErr, preserveErr)
			}
			after, ok, err := d.GetSetting(key)
			if err != nil || !ok || after != string(payload) {
				t.Fatalf("v2 屏障原始字节未保留: present=%v err=%v", ok, err)
			}
			state.Revision++
			if err := d.SetXiequWhitelistState(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			state.Revision++
		}
	}
}
