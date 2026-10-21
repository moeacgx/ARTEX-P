package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 回归：密钥只写入，不出现在读取响应或数据库明文中，普通开关保存不能抹掉它。
func TestFOFASettingsSecretLifecycle(t *testing.T) {
	s := newModelExecutionServer(t)
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`) })
	api := modelHTTPClient(t, s)
	got := api("GET", "/api/integrations/fofa", "", 200)
	if got["enabled"] != false || got["api_key_set"] != false {
		t.Fatal(got)
	}
	api("PUT", "/api/integrations/fofa", `{"enabled":true}`, 400)
	got = api("PUT", "/api/integrations/fofa", `{"api_key":"test-key-must-not-echo","enabled":true}`, 200)
	if got["enabled"] != true || got["api_key_set"] != true {
		t.Fatal(got)
	}
	raw, found, err := s.m.pg.GetSetting("integration.fofa.v1")
	if err != nil || !found || strings.Contains(raw, "test-key-must-not-echo") {
		t.Fatal("配置未加密保存")
	}
	got = api("PUT", "/api/integrations/fofa", `{"enabled":false}`, 200)
	if got["api_key_set"] != true {
		t.Fatal("开关修改清除了密钥")
	}
	// 新的服务对象读取同一持久配置，不能依赖启动时内存快照。
	restarted := &Server{m: s.m, ctx: s.ctx, jwtKey: s.jwtKey}
	got = modelHTTPClient(t, restarted)("GET", "/api/integrations/fofa", "", 200)
	if got["api_key_set"] != true || got["enabled"] != false {
		t.Fatal(got)
	}
	got = api("PUT", "/api/integrations/fofa", `{"api_key":""}`, 200)
	if got["api_key_set"] != false || got["enabled"] != false {
		t.Fatal("清除密钥未禁用功能")
	}
}

func TestFOFAConnectionTestHoldsSessionGuardWithoutOrphanActivity(t *testing.T) {
	s := newModelExecutionServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(502) }))
	defer proxy.Close()
	defer close(release)
	p := createRuntimeProxy(t, s, proxy.URL)
	previous, err := s.m.pg.GlobalProxyID()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.SetGlobalProxyID(&p.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetGlobalProxyID(previous) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/integrations/fofa/test", strings.NewReader(`{"api_key":"draft-only"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() { s.testFOFASettings(rr, req); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("连接测试没有使用选定全局代理")
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM warp_proxy_health_activity WHERE proxy_id=$1`, p.ID).Scan(&count); err != nil || count != 0 {
		cancel()
		<-done
		t.Fatalf("管理检测写入了无法按主体恢复的持久活动: %d %v", count, err)
	}
	if guard, e := s.m.pg.TryWarpHealthExecution(t.Context(), p.ID); e == nil {
		_ = guard.Close()
		t.Fatal("检测仍在执行时允许同出口维护")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消未结束检测")
	}
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM warp_proxy_health_activity WHERE proxy_id=$1`, p.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("取消后未释放出口活动")
	}
	guard, err := s.m.pg.TryWarpHealthExecution(t.Context(), p.ID)
	if err != nil {
		t.Fatal("检测退出后会话锁未释放")
	}
	_ = guard.Close()
}

func TestFOFAConnectionTestUsesDraftWithoutSavingOrSearching(t *testing.T) {
	s := newModelExecutionServer(t)
	previous, _ := s.m.pg.GlobalProxyID()
	_ = s.m.pg.SetGlobalProxyID(nil)
	t.Cleanup(func() {
		_ = s.m.pg.SetGlobalProxyID(previous)
		_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`)
	})
	keys := []string{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info/my" {
			t.Error("连接测试消耗了资产搜索")
		}
		keys = append(keys, r.URL.Query().Get("key"))
		fmt.Fprint(w, `{"error":false,"email":"private@example.test"}`)
	}))
	defer upstream.Close()
	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1"
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "https://"))
	}
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old; tr.CloseIdleConnections() })
	key := "stored-key"
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"api_key":"draft-key"}`} {
		rr := httptest.NewRecorder()
		s.testFOFASettings(rr, httptest.NewRequest("POST", "/api/integrations/fofa/test", strings.NewReader(body)))
		if rr.Code != 200 || strings.Contains(rr.Body.String(), "private@example") {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	config, err := s.m.pg.FOFAConfig()
	if err != nil || config.APIKey != key || len(keys) != 2 || keys[0] != "stored-key" || keys[1] != "draft-key" {
		t.Fatal("测试选择Key错误或擅自保存草稿")
	}
}

func TestFOFASettingsRejectInvalidAndUnauthenticatedRequests(t *testing.T) {
	s := newModelExecutionServer(t)
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`) })
	api := modelHTTPClient(t, s)
	for _, body := range []string{`null`, `[]`, `{"api_key":123}`, `{"enabled":"true"}`, `{"api_key":"x","extra":"secret"}`, `{} {}`, `{"api_key":"a\nb"}`} {
		api("PUT", "/api/integrations/fofa", body, 400)
	}
	api("POST", "/api/integrations/fofa/test", `{}`, 400)
	for _, path := range []string{"/api/integrations/fofa", "/api/integrations/fofa/test"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("未登录访问 %s: %d", path, rr.Code)
		}
	}
	encoded, _ := json.Marshal(api("GET", "/api/integrations/fofa", "", 200))
	if strings.Contains(string(encoded), "api_key\"") {
		t.Fatal("响应暴露密钥字段")
	}
}
