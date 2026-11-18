package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestXiequOperationMissingStateStillReportsBarrier(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, key := "synthetic-account", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	op := XiequOperation{Token: "pending-token", Scope: c.WhitelistScope(), Kind: "add", IP: "203.0.113.99", Memo: "pending-marker", Account: c.accountIdentity()}
	payload, _ := json.Marshal(op)
	if _, err := d.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", xiequOperationKey(c), string(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DELETE FROM settings WHERE key=$1", settingXiequWhitelistState); err != nil {
		t.Fatal(err)
	}
	state, err := d.XiequWhitelistState()
	if err != nil || state.LastStatus != "operation_unknown" || state.Scope != c.WhitelistScope() {
		t.Fatalf("状态行缺失隐去账号屏障: %+v %v", state, err)
	}
	if _, err := d.Exec("UPDATE settings SET value='broken' WHERE key=$1", xiequOperationKey(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.XiequWhitelistState(); err == nil {
		t.Fatal("损坏屏障被忽略")
	}
}

func TestXiequLegacyPendingReplacesCompletedJournal(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, key := "synthetic-account", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	old := XiequOperation{Token: "old-token", Scope: c.WhitelistScope(), Kind: "add", IP: "203.0.113.98", Memo: "old-marker", Completed: true, Account: c.accountIdentity()}
	payload, _ := json.Marshal(old)
	if _, err := d.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", xiequOperationKey(c), string(payload)); err != nil {
		t.Fatal(err)
	}
	s := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.99", Memo: "legacy-marker"}}
	if err := d.SetXiequWhitelistState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := d.PreserveXiequPending(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	op, err := d.XiequOperation(t.Context(), c)
	if err != nil || op == nil || op.Memo != "legacy-marker" {
		t.Fatalf("旧完成记录遮蔽遗留 pending: %+v %v", op, err)
	}
}

func TestXiequOperationTokenCASAndNormalCompletion(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, key := "synthetic-account", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	s := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.99", Memo: "owned-token", Prepared: true}}
	if err := d.SetXiequWhitelistState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Revision++
	op := XiequOperation{Token: "operation-token", Scope: s.Scope, Kind: "add", IP: s.PendingAdd.IP, Memo: s.PendingAdd.Memo}
	if err := d.ClaimXiequOperation(t.Context(), c, s, op); err != nil {
		t.Fatal(err)
	}
	s.Revision++
	if err := d.ClaimXiequOperation(t.Context(), c, s, op); !errors.Is(err, ErrXiequOperationPending) {
		t.Fatalf("重复占用=%v", err)
	}
	wrong := op
	wrong.Token = "stale-owner"
	if err := d.CompleteXiequOperation(t.Context(), c, s, wrong); !errors.Is(err, ErrXiequOperationPending) {
		t.Fatalf("错误 token 释放=%v", err)
	}
	s.PendingAdd = nil
	if err := d.CompleteXiequOperation(t.Context(), c, s, op); err != nil {
		t.Fatal(err)
	}
	s.Revision++
	if active, err := d.XiequOperation(t.Context(), c); err != nil || active != nil {
		t.Fatalf("正常完成未释放: %+v %v", active, err)
	}
	if err := d.CompleteXiequOperation(t.Context(), c, s, op); !errors.Is(err, ErrXiequOperationPending) {
		t.Fatalf("重复完成=%v", err)
	}
	raw, ok, err := d.GetSetting(xiequOperationKey(c))
	if err != nil || !ok || !strings.Contains(raw, `"completed":true`) || strings.Contains(raw, uid) || strings.Contains(raw, key) || strings.Contains(xiequOperationKey(c), uid) {
		t.Fatalf("完成凭据或摘要键不正确: %v", err)
	}
}

func TestXiequOperationSurvivesIdentityRevisionAndABA(t *testing.T) {
	for _, change := range []string{"ukey", "aba", "base-aba"} {
		t.Run(change, func(t *testing.T) {
			d, _ := fofaSettingsTestDB(t)
			uid, key := "account-a", "synthetic-key"
			c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &key})
			if err != nil {
				t.Fatal(err)
			}
			s := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.99", Memo: "original-marker", Prepared: true}}
			if err := d.SetXiequWhitelistState(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			s.Revision++
			op := XiequOperation{Token: "original-token", Scope: s.Scope, Kind: "add", IP: s.PendingAdd.IP, Memo: s.PendingAdd.Memo}
			if err := d.ClaimXiequOperation(t.Context(), c, s, op); err != nil {
				t.Fatal(err)
			}
			s.Revision++
			var changed XiequConfig
			switch change {
			case "ukey":
				key = "rotated-key"
				changed, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{UKEY: &key})
			case "aba":
				other := "account-b"
				changed, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &other})
				if err == nil {
					if barrier, e := d.XiequOperation(t.Context(), changed); e != nil || barrier != nil {
						t.Fatalf("B 被 A 屏障污染: %+v %v", barrier, e)
					}
				}
				if err == nil {
					changed, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid})
				}
			case "base-aba":
				other := "https://other.invalid/api"
				_, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{BaseURL: &other})
				if err == nil {
					changed, err = d.SetXiequConfig(t.Context(), XiequConfigPatch{BaseURL: &c.BaseURL})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed.WhitelistScope() == c.WhitelistScope() {
				t.Fatal("配置代次未隔离")
			}
			barrier, err := d.XiequOperation(t.Context(), changed)
			if err != nil || barrier == nil || barrier.Token != op.Token || barrier.Memo != op.Memo {
				t.Fatalf("换代绕过旧屏障: %+v %v", barrier, err)
			}
			if err := d.CompleteXiequOperation(t.Context(), c, s, op); !errors.Is(err, ErrXiequConfigChanged) {
				t.Fatalf("旧身份完成=%v", err)
			}
			fresh, err := d.XiequWhitelistState()
			if err != nil {
				t.Fatal(err)
			}
			fresh.PendingAdd = &XiequPendingAdd{IP: op.IP, Memo: "new-marker", Prepared: true}
			if err := d.SetXiequWhitelistState(t.Context(), fresh); err != nil {
				t.Fatal(err)
			}
			fresh.Revision++
			newOp := op
			newOp.Scope, newOp.Token, newOp.Memo = changed.WhitelistScope(), "new-token", "new-marker"
			if err := d.ClaimXiequOperation(t.Context(), changed, fresh, newOp); !errors.Is(err, ErrXiequOperationPending) {
				t.Fatalf("新代次突破账号屏障=%v", err)
			}
		})
	}
}

func TestXiequLegacyPendingBarrierSurvivesConfigChange(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	uid, key := "account-a", "synthetic-key"
	c, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UID: &uid, UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	s := XiequWhitelistState{Scope: c.WhitelistScope(), PendingAdd: &XiequPendingAdd{IP: "203.0.113.99", Memo: "legacy-marker"}}
	if err := d.SetXiequWhitelistState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	key = "rotated-key"
	changed, err := d.SetXiequConfig(t.Context(), XiequConfigPatch{UKEY: &key})
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.XiequOperation(t.Context(), changed)
	if err != nil || op == nil || op.Memo != "legacy-marker" || op.Scope != c.WhitelistScope() {
		t.Fatalf("旧 pending 在换代时遗失: %+v %v", op, err)
	}
}
