package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

// 仅使用本任务独占端口与库，每例隔离 schema，不触碰其他工人的数据库。
func xiequIsolatedManager(t *testing.T) *Manager {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("需要显式配置隔离 PostgreSQL fixture")
	}
	u, err := url.Parse(dsn)
	if err != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() ||
		!strings.HasPrefix(u.Path, "/artex_") || !strings.HasSuffix(u.Path, "_test") ||
		u.RawQuery != "sslmode=disable" || u.Fragment != "" {
		t.Fatal("Xiequ 集成测试只接受显式本地 artex_*_test 库且禁止参数覆盖")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("xiequ_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	pool, err := sql.Open("pgx", dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err := pool.Exec(`CREATE TABLE settings(key text PRIMARY KEY,value text NOT NULL,updated_at timestamptz DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureProxySecrets(filepath.Join(t.TempDir(), "key")); err != nil {
		t.Fatal(err)
	}
	return &Manager{pg: &db.DB{DB: pool}}
}

func xiequConfigureFixture(t *testing.T, m *Manager, upstream *httptest.Server) db.XiequConfig {
	t.Helper()
	oldClient, oldProbe := newXiequClient, discoverXiequPublicIP
	newXiequClient = func(base, uid, key string) *xiequ.Client {
		return xiequ.NewWithHTTPClient(base, uid, key, upstream.Client())
	}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.99", nil }
	t.Cleanup(func() { newXiequClient = oldClient; discoverXiequPublicIP = oldProbe })
	uid, key, enabled, base := "synthetic-account", "synthetic-key", true, upstream.URL
	c, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &uid, UKEY: &key, Enabled: &enabled, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestXiequPendingAddRecoversAfterLostResponsesAndRestart(t *testing.T) {
	m := xiequIsolatedManager(t)
	var mu sync.Mutex
	entries := []xiequ.Entry{{IP: "203.0.113.1", Memo: "manual"}, {IP: "203.0.113.2", Memo: "manual"}, {IP: "203.0.113.3", Memo: "manual"}, {IP: "203.0.113.4", Memo: "manual"}}
	failLists := 0
	var deleted []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Query().Get("act") {
		case "getjson":
			if failLists > 0 {
				failLists--
				http.Error(w, "lost verification", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			if len(entries) < 5 {
				var raw string
				if err := m.pg.QueryRow(`SELECT value FROM settings WHERE key='integration.xiequ.whitelist.v1'`).Scan(&raw); err != nil {
					t.Error(err)
				}
				if !strings.Contains(raw, `"pending_add"`) {
					t.Error("Add发送前未持久化pending-add")
				}
				entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
				failLists = 2
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			_, _ = w.Write([]byte("quota full"))
		case "del":
			ip := r.URL.Query().Get("ip")
			deleted = append(deleted, ip)
			for i, e := range entries {
				if e.IP == ip {
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	xiequConfigureFixture(t, m, upstream)
	result, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || result.OK {
		t.Fatalf("首次不应误报成功: %+v %v", result, err)
	}
	restarted := &Manager{pg: m.pg}
	result, err = restarted.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !result.OK || len(result.ManagedIPs) != 1 || result.ManagedIPs[0] != "203.0.113.99" {
		t.Fatalf("重启未恢复provenance: %+v %v", result, err)
	}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
	result, err = restarted.refreshXiequWhitelist(t.Context(), true)
	if err != nil || len(deleted) != 1 || deleted[0] != "203.0.113.99" {
		t.Fatalf("未只回收本安装添加: deleted=%v result=%+v err=%v", deleted, result, err)
	}
}

func TestXiequAttemptDeadlineStillEnforcesCooldown(t *testing.T) {
	m := xiequIsolatedManager(t)
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	defer upstream.Close()
	xiequConfigureFixture(t, m, upstream)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result, err := m.refreshXiequWhitelist(ctx, false)
	if err != nil || result.Reason != "list_failed" {
		t.Fatalf("首次结果: %+v %v", result, err)
	}
	state, err := m.pg.XiequWhitelistState()
	if err != nil || state.LastAttemptAt == nil {
		t.Fatalf("取消后缺少attempt: %+v %v", state, err)
	}
	result, err = m.refreshXiequWhitelist(t.Context(), false)
	if err != nil || result.Reason != "cooldown" || calls.Load() != 1 {
		t.Fatalf("取消绕过cooldown: %+v %v calls=%d", result, err, calls.Load())
	}
}

func TestXiequRefreshSerializesAcrossManagers(t *testing.T) {
	m := xiequIsolatedManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_ = json.NewEncoder(w).Encode([]xiequ.Entry{{IP: "203.0.113.99", Memo: "manual"}})
	}))
	defer func() { once.Do(func() { close(release) }); upstream.Close() }()
	xiequConfigureFixture(t, m, upstream)
	done := make(chan error, 1)
	go func() { _, err := m.refreshXiequWhitelist(t.Context(), true); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("首个刷新未发起")
	}
	result, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || result.Reason != "busy" {
		t.Errorf("并发刷新应被阻止: %+v %v", result, err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("首个刷新未退出")
	}
	if calls.Load() != 1 {
		t.Errorf("并发发出%d次请求", calls.Load())
	}
}

func TestXiequPendingRecoveryDoesNotClaimManualSameIP(t *testing.T) {
	m := xiequIsolatedManager(t)
	var deletes atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "del" {
			deletes.Add(1)
		}
		_ = json.NewEncoder(w).Encode([]xiequ.Entry{{IP: "203.0.113.99", Memo: "manual"}, {IP: "203.0.113.2"}, {IP: "203.0.113.3"}, {IP: "203.0.113.4"}, {IP: "203.0.113.5"}})
	}))
	defer upstream.Close()
	config := xiequConfigureFixture(t, m, upstream)
	if err := m.pg.SetXiequWhitelistState(t.Context(), db.XiequWhitelistState{Scope: config.WhitelistScope(), PendingAdd: &db.XiequPendingAdd{IP: "203.0.113.99", Memo: "ARTEX [artex:other-operation]"}}); err != nil {
		t.Fatal(err)
	}
	result, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !result.OK || len(result.ManagedIPs) != 0 {
		t.Fatalf("误认领手工同IP: %+v %v", result, err)
	}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
	result, err = m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || result.Reason != "quota_full" || deletes.Load() != 0 {
		t.Fatalf("手工满配额被误删: %+v %v deletes=%d", result, err, deletes.Load())
	}
}

func TestXiequCanceledOldIdentityCannotOverwriteNewState(t *testing.T) {
	m := xiequIsolatedManager(t)
	entered := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer upstream.Close()
	xiequConfigureFixture(t, m, upstream)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.refreshXiequWhitelist(ctx, false); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("旧请求未发起")
	}
	uid := "new-account"
	current, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetXiequWhitelistState(t.Context(), db.XiequWhitelistState{Scope: current.WhitelistScope(), LastStatus: "new-identity"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("旧取消请求未退出")
	}
	state, err := m.pg.XiequWhitelistState()
	if err != nil || state.LastStatus != "new-identity" || state.LastAttemptAt != nil {
		t.Fatalf("独立保存覆盖新身份: %+v %v", state, err)
	}
}

func TestXiequSettingsRouteFreshAndCorruptState(t *testing.T) {
	m := xiequIsolatedManager(t)
	s := &Server{m: m}
	mux := http.NewServeMux()
	s.registerXiequRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/integrations/xiequ", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"managed_ips":[]`) {
		t.Fatalf("fresh契约失效: %d %s", response.Code, response.Body.String())
	}
	if _, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec(`UPDATE settings SET value='invalid-json' WHERE key='integration.xiequ.whitelist.v1'`); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/integrations/xiequ", strings.NewReader(`{}`)))
	if response.Code != 500 || !strings.Contains(response.Body.String(), "状态读取失败") {
		t.Fatalf("真实存储错误被隐藏: %d %s", response.Code, response.Body.String())
	}
}

func TestXiequLegacyHTTPConfigNeverSendsCredentials(t *testing.T) {
	m := xiequIsolatedManager(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`[]`)) }))
	defer upstream.Close()
	uid, key, enabled := "synthetic-account", "synthetic-key", true
	_, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &uid, UKEY: &key, Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := m.pg.QueryRow(`SELECT value FROM settings WHERE key='integration.xiequ.v1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	stored["base_url"] = upstream.URL
	payload, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec(`UPDATE settings SET value=$1 WHERE key='integration.xiequ.v1'`, string(payload)); err != nil {
		t.Fatal(err)
	}
	result, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || result.OK || result.Reason != "list_failed" || calls.Load() != 0 {
		t.Fatalf("历史HTTP配置发出请求: %+v %v calls=%d", result, err, calls.Load())
	}
	if !strings.Contains(result.Message, "失败") {
		t.Fatal("未显式安全失败")
	}
}
