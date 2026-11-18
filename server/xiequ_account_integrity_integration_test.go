package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

func TestXiequCorruptLegacyAccountCannotSendAdd(t *testing.T) {
	m := xiequIsolatedManager(t)
	var adds atomic.Int32
	var deletes atomic.Int32
	var addedEntry xiequ.Entry
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			entries := []xiequ.Entry{}
			if adds.Load() != 0 {
				entries = append(entries, addedEntry)
			}
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			addedEntry = xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")}
			adds.Add(1)
			_, _ = w.Write([]byte("ok"))
		case "del":
			deletes.Add(1)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()

	c := xiequConfigureFixture(t, m, upstream)
	address := upstream.URL + "/api"
	c, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{BaseURL: &address})
	if err != nil {
		t.Fatal(err)
	}

	legacyURL := upstream.URL + "/%61pi"
	sum := sha256.Sum256([]byte(c.UID + "\n" + legacyURL))
	legacyIdentity := hex.EncodeToString(sum[:])
	key := "integration.xiequ.operation.v1:" + legacyIdentity
	original := map[string]any{
		"token":   "preserve-original-token",
		"scope":   legacyIdentity + ":" + c.Revision,
		"kind":    "del",
		"ip":      "203.0.113.1",
		"memo":    "preserve-original-marker",
		"account": strings.Repeat("z", 64),
	}
	wire, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", key, string(wire)); err != nil {
		t.Fatal(err)
	}

	result, refreshErr := m.refreshXiequWhitelist(t.Context(), true)
	after, ok, readErr := m.pg.GetSetting(key)
	if readErr != nil || !ok || after != string(wire) {
		t.Fatalf("未知屏障原始字节/token/scope/marker 未保留: present=%v readErr=%v", ok, readErr)
	}
	t.Logf("原 journal 字节保持=true adds=%d deletes=%d result=%+v err=%v", adds.Load(), deletes.Load(), result, refreshErr)
	if refreshErr == nil || result.OK || adds.Load() != 0 || deletes.Load() != 0 {
		t.Fatalf("损坏旧账号未阻止远端变更: result=%+v err=%v adds=%d deletes=%d", result, refreshErr, adds.Load(), deletes.Load())
	}
}
