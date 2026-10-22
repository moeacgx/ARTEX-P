package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGlobalWarpHealthPolicyHTTP(t *testing.T) {
	s := catalogTestServer(t)
	if err := s.m.pg.MigrateWarpHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterProxyActionRoutes(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/api/proxies/warp-health-policy", "")
	var p globalWarpHealthPolicyDTO
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != 200 || p.Scope != "global" || p.Revision != 1 || !p.AutoRecoverEnabled {
		t.Fatalf("GET无效：%d %s %v", w.Code, w.Body, err)
	}
	for _, body := range []string{`{}`, `{"expected_revision":1}`, `{"expected_revision":0,"auto_recover_enabled":true}`, `{"expected_revision":1,"auto_recover_enabled":"true"}`, `{"expected_revision":1,"auto_recover_enabled":true,"force":true}`, `{"expected_revision":9007199254740992,"auto_recover_enabled":true}`} {
		w = call("PUT", "/api/proxies/warp-health-policy", body)
		if w.Code != 400 {
			t.Fatalf("非法全局策略未拒绝：%d", w.Code)
		}
	}
	w = call("PUT", "/api/proxies/warp-health-policy", `{"expected_revision":1,"auto_recover_enabled":false}`)
	if w.Code != 200 {
		t.Fatalf("CAS保存失败：%d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Revision != 2 || p.AutoRecoverEnabled {
		t.Fatalf("CAS响应不是已提交版本：%+v %v", p, err)
	}
	w = call("PUT", "/api/proxies/warp-health-policy", `{"expected_revision":1,"auto_recover_enabled":true}`)
	var conflict map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &conflict)
	if w.Code != 409 || conflict["current_revision"] != float64(2) || conflict["code"] != "warp_health_revision_conflict" {
		t.Fatalf("CAS冲突信息无效：%d %s", w.Code, w.Body)
	}
	p2, _ := s.m.pg.GlobalWarpHealthPolicy(t.Context())
	if p2.AutoRecoverEnabled || p2.Revision != 2 {
		t.Fatal("冲突写修改了权威策略")
	}
	var id int64
	if err := s.m.pg.QueryRow(`INSERT INTO outbound_proxies(name,kind,enabled,status) VALUES('全局测试','warp',true,'healthy') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	w = call("PUT", "/api/proxies/"+i64s(id)+"/health-policy", `{"expected_revision":1,"auto_recover_enabled":true}`)
	if w.Code != 410 {
		t.Fatalf("旧节点写接口仍可修改策略：%d %s", w.Code, w.Body)
	}
}
