package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

func TestXiequRefreshAccountSwitchProtectsManualQuotaAndNewState(t *testing.T) {
	m := xiequIsolatedManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var deletes atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, action := r.URL.Query().Get("uid"), r.URL.Query().Get("act")
		if uid == "old-account" && action == "getjson" {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
		if action == "del" {
			deletes.Add(1)
		}
		if action == "getjson" {
			ips := []map[string]string{{"IP": "203.0.113.8"}, {"IP": "203.0.113.2"}, {"IP": "203.0.113.3"}, {"IP": "203.0.113.4"}, {"IP": "203.0.113.5"}}
			if uid == "old-account" {
				ips = []map[string]string{{"IP": "203.0.113.99"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": ips})
			return
		}
		_, _ = w.Write([]byte("quota full"))
	}))
	defer func() { once.Do(func() { close(release) }); upstream.Close() }()
	previousClient := newXiequClient
	newXiequClient = func(base, uid, key string) *xiequ.Client {
		return xiequ.NewWithHTTPClient(base, uid, key, upstream.Client())
	}
	defer func() { newXiequClient = previousClient }()
	previous := discoverXiequPublicIP
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.99", nil }
	defer func() { discoverXiequPublicIP = previous }()
	uid, key, enabled, base := "old-account", "synthetic-key", true, upstream.URL
	old, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &uid, UKEY: &key, Enabled: &enabled, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.pg.SetXiequWhitelistState(t.Context(), db.XiequWhitelistState{Scope: old.WhitelistScope(), ManagedIPs: []string{"203.0.113.8"}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.refreshXiequWhitelist(t.Context(), true); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("旧刷新没有进入网络请求")
	}
	uid = "new-account"
	current, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	newState := db.XiequWhitelistState{Scope: current.WhitelistScope(), LastStatus: "new-identity"}
	if err = m.pg.SetXiequWhitelistState(t.Context(), newState); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("旧刷新没有退出")
	}
	state, err := m.pg.XiequWhitelistState()
	if err != nil || state.LastStatus != "new-identity" || len(state.ManagedIPs) != 0 {
		t.Fatalf("旧刷新覆盖新状态: %+v %v", state, err)
	}
	result, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || result.Reason != "quota_full" {
		t.Fatalf("满配额结果=%+v %v", result, err)
	}
	if deletes.Load() != 0 {
		t.Fatalf("新账号手工 IP 被误删: %d", deletes.Load())
	}
}
