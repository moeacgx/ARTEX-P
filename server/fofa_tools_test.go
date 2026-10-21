package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

func TestFOFAToolAvailableToBoundAgents(t *testing.T) {
	s := newModelExecutionServer(t)
	key, on := "test-key-must-not-echo", true
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &on, APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`)
		_, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key IN ('fofa_search','fofa_status')`)
	})
	tools, _ := s.hostTools()
	found := false
	for _, tool := range tools {
		if tool.Name() == "fofa_search" {
			found = true
		}
	}
	if !found {
		t.Fatal("已启用 FOFA，但 Agent 工具集中缺少 fofa_search")
	}
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	oldResolve := agent.ToolResolve
	t.Cleanup(func() { agent.ToolResolve = oldResolve })
	wireTools(s.m.pg, nil)
	var cached actool.CoreTool
	for _, agentKey := range []string{"worker", "auto", "pentest", "planner", "mainagent", "goals"} {
		resolved := agent.ToolResolve(t.Context(), agentKey, append([]actool.CoreTool(nil), tools...))
		has := false
		for _, tool := range resolved {
			if tool.Name() == "fofa_search" {
				has = true
				cached = tool
			}
		}
		want := agentKey == "worker" || agentKey == "auto" || agentKey == "pentest"
		if has != want {
			t.Fatalf("Agent %s FOFA绑定错误: %v", agentKey, has)
		}
	}
	if _, err := s.m.pg.Exec(`UPDATE tools SET agents='[]',enabled=false WHERE key='fofa_search'`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	row, err := s.m.pg.GetTool("fofa_search")
	if err != nil || row.Enabled || len(row.Agents) != 0 {
		t.Fatal("重启播种覆盖了用户解绑/禁用")
	}
	result, _ := cached.Call(t.Context(), []byte(`{"query":"domain=\"example.test\""}`), nil)
	if !result.IsError || !strings.Contains(result.Flatten(), "权限") {
		t.Fatal("缓存工具未检查最新绑定")
	}
	_, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key='fofa_search'`)
	for _, tool := range agent.ToolResolve(t.Context(), "auto", append([]actool.CoreTool(nil), tools...)) {
		if tool.Name() == "fofa_search" {
			t.Fatal("目录缺失时默认放行")
		}
	}
	if err := s.m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tool := range agent.ToolResolve(t.Context(), "auto", append([]actool.CoreTool(nil), tools...)) {
		if tool.Name() == "fofa_search" {
			t.Fatal("数据库故障时默认放行")
		}
	}
}

// 请求必须穿过任务选定代理，不能穿过明文流量录制器；结束凭证不能继续使用。
func TestFOFAToolSearchUsesAdmittedProxyWithoutCapture(t *testing.T) {
	s := newModelExecutionServer(t)
	var calls, connects atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("key") != "test-key-must-not-echo" || r.URL.Path != "/api/v1/search/all" {
			t.Error("查询未使用服务端配置")
		}
		fields := strings.Split(r.URL.Query().Get("fields"), ",")
		if len(fields) == 8 {
			query, _ := base64.StdEncoding.DecodeString(r.URL.Query().Get("qbase64"))
			if string(query) != `body="dujiaoka"` || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("size") != "50" {
				t.Error("显式查询参数未原样传至上游")
			}
		}
		values := map[string]string{"host": "example.test", "ip": "192.0.2.1", "port": "443", "protocol": "https", "domain": "example.test", "title": "sample", "server": "test", "country_name": "test-region"}
		var row []string
		for _, field := range fields {
			row = append(row, values[field])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": false, "page": 1, "size": 1, "results": [][]string{row}})
	}))
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "fofa.info:443" {
			t.Error("出口未收到固定 FOFA HTTPS 请求")
			w.WriteHeader(502)
			return
		}
		connects.Add(1)
		dst, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "https://"))
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer dst.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(dst, conn); _ = dst.Close(); close(done) }()
		_, _ = io.Copy(conn, dst)
		_ = conn.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	oldTransport := http.DefaultTransport
	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1"
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = oldTransport; tr.CloseIdleConnections() })
	p := createRuntimeProxy(t, s, proxy.URL)
	task := createRuntimeTask(t, s, db.TaskProxyModeFixed, &p.ID)
	capture, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.m.traffic, s.m.trafficOn = capture, true
	t.Cleanup(func() { s.m.traffic = nil; s.m.trafficOn = false; capture.Close() })
	key, on := "test-key-must-not-echo", true
	if _, err = s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &on, APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`)
		_, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key IN ('fofa_search','fofa_status')`)
	})
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	oldResolve, oldTrafficBinding := agent.ToolResolve, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() { agent.ToolResolve, agent.FindingTrafficBindingEnabled = oldResolve, oldTrafficBinding })
	wireTools(s.m.pg, nil)
	tools, _ := s.hostTools()
	var selected actool.CoreTool
	for _, tool := range agent.ToolResolve(t.Context(), "auto", tools) {
		if tool.Name() == "fofa_search" {
			selected = tool
		}
	}
	if selected == nil {
		t.Fatal("FOFA 工具不存在")
	}
	result, _ := selected.Call(context.Background(), []byte(`{"query":"domain=\"example.test\""}`), nil)
	if !result.IsError || calls.Load() != 0 {
		t.Fatal("无准入凭证时发起了网络请求")
	}
	ctx, finish, ok := s.engine.beginTaskOperationContext(t.Context(), task.ID)
	if !ok {
		t.Fatal("测试任务准入失败")
	}
	defer finish()
	result, err = selected.Call(ctx, []byte(`{"query":"domain=\"example.test\""}`), nil)
	if err != nil || result.IsError || !strings.Contains(result.Flatten(), "192.0.2.1") || strings.Contains(result.Flatten(), key) {
		t.Fatalf("搜索失败或泄密: %v %s", err, result.Flatten())
	}
	if calls.Load() != 1 || connects.Load() != 1 {
		t.Fatal("请求未严格通过任务代理一次执行")
	}
	result, err = selected.Call(ctx, []byte(`{"query":"body=\"dujiaoka\"","page":1,"size":50,"fields":["host","ip","port","protocol","domain","title","server","country_name"]}`), nil)
	if err != nil || result.IsError || !strings.Contains(result.Flatten(), `"country_name":"test-region"`) || strings.Contains(result.Flatten(), key) {
		t.Fatalf("合法字段数组未通过完整装配及上游查询: %v %s", err, result.Flatten())
	}
	if calls.Load() != 2 || connects.Load() != 2 {
		t.Fatal("显式字段查询未严格执行一次")
	}
	if n, err := capture.Count(); err != nil || n != 0 {
		t.Fatal("凭据请求进入了流量库")
	}
	finish()
	result, _ = selected.Call(ctx, []byte(`{"query":"domain=\"example.test\""}`), nil)
	if !result.IsError || calls.Load() != 2 {
		t.Fatal("已结束凭证仍能继续调用")
	}
}
