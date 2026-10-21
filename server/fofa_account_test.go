package server

import (
	"context"
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

// 保持官方目标域名，只有测试拨号被重定向到本地 TLS 服务。
func localFOFAAccountUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1"
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "https://"))
	}
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old; tr.CloseIdleConnections() })
}

func TestFOFAAccountQuotasUseSelectedKeyAndWhitelist(t *testing.T) {
	s := newModelExecutionServer(t)
	key := fmt.Sprintf("synthetic-account-%d", time.Now().UnixNano())
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`) })
	rawBefore, _, err := s.m.pg.GetSetting("integration.fofa.v1")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	localFOFAAccountUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "fofa.info" || r.URL.Path != "/api/v1/info/my" {
			t.Error("额度查询访问了非账户接口")
		}
		keys = append(keys, r.URL.Query().Get("key"))
		fmt.Fprint(w, `{"error":false,"fofa_point":49200.5,"remain_free_point":0,"remain_api_query":49992,"remain_api_data":499398,"fcoin":987,"email":"private@example.test","key":"do-not-return","avatar":"private"}`)
	})
	for _, tc := range []struct{ body, source string }{{`{}`, "saved"}, {`{"api_key":"synthetic-draft-account"}`, "draft"}} {
		rr := httptest.NewRecorder()
		s.testFOFASettings(rr, httptest.NewRequest(http.MethodPost, "/api/integrations/fofa/test", strings.NewReader(tc.body)))
		var result struct {
			OK        bool           `json:"ok"`
			Account   map[string]any `json:"account"`
			CheckedAt string         `json:"checked_at"`
			KeySource string         `json:"key_source"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil || !result.OK {
			t.Fatalf("账户查询失败: %d %s", rr.Code, rr.Body.String())
		}
		if len(result.Account) != 4 || result.Account["fofa_point"] != 49200.5 || result.Account["remain_free_point"] != float64(0) || result.Account["remain_api_query"] != float64(49992) || result.Account["remain_api_data"] != float64(499398) {
			t.Fatalf("缺少或误映射额度: %+v", result.Account)
		}
		at, err := time.Parse(time.RFC3339Nano, result.CheckedAt)
		if err != nil || time.Since(at) > 5*time.Second || result.KeySource != tc.source {
			t.Fatalf("读取时间或Key来源不准确: %q %q", result.CheckedAt, result.KeySource)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("账户额度响应应禁止缓存")
		}
		for _, secret := range []string{key, "synthetic-draft-account", "private@example.test", "do-not-return", "\"fcoin\""} {
			if strings.Contains(rr.Body.String(), secret) {
				t.Fatal("非额度字段或密钥进入响应")
			}
		}
	}
	if len(keys) != 2 || keys[0] != key || keys[1] != "synthetic-draft-account" {
		t.Fatal("账户查询Key来源错误或额外发起请求")
	}
	rawAfter, _, err := s.m.pg.GetSetting("integration.fofa.v1")
	if err != nil || rawAfter != rawBefore {
		t.Fatal("查询额度修改了保存的配置")
	}
}

func TestFOFAAccountUnknownQuotasAndRateLimit(t *testing.T) {
	s := newModelExecutionServer(t)
	key := fmt.Sprintf("synthetic-partial-account-%d", time.Now().UnixNano())
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`) })
	calls := 0
	localFOFAAccountUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"error":false,"fofa_point":0,"remain_free_point":null}`)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	})
	call := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.testFOFASettings(rr, httptest.NewRequest(http.MethodPost, "/api/integrations/fofa/test", strings.NewReader(`{}`)))
		return rr
	}
	rr := call()
	var result map[string]any
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil {
		t.Fatal("部分额度响应失败")
	}
	account, ok := result["account"].(map[string]any)
	if !ok || account["fofa_point"] != float64(0) || account["remain_free_point"] != nil || account["remain_api_query"] != nil || account["remain_api_data"] != nil {
		t.Fatal("未知额度被当作零或真实零被丢失")
	}
	rr = call()
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "120" || strings.Contains(rr.Body.String(), "account") {
		t.Fatalf("限流错误不应携带旧额度: %d %s", rr.Code, rr.Body.String())
	}
	rr = call()
	if rr.Code != 429 || calls != 2 {
		t.Fatal("额度刷新绕过已记录冷却")
	}
}
