package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

func TestXiequManagedIPHelpers(t *testing.T) {
	managed := appendXiequManagedIP([]string{"203.0.113.7"}, "203.0.113.8")
	if len(managed) != 2 {
		t.Fatalf("追加管理 IP 失败: %#v", managed)
	}
	if got := appendXiequManagedIP(managed, "203.0.113.7"); len(got) != 2 {
		t.Fatalf("管理 IP 重复追加: %#v", got)
	}
}

func TestXiequQuotaRulesAreExplicit(t *testing.T) {
	for _, fragment := range []string{"5 个", "按次计费", "流量计费", "包时", "包日", "包月", "注册新账户"} {
		if !strings.Contains(xiequQuotaMessage, fragment) {
			t.Fatalf("配额说明缺少 %q: %s", fragment, xiequQuotaMessage)
		}
	}
}

func TestXiequQuotaCleanupOnlyDeletesManagedStaleIPs(t *testing.T) {
	m := xiequIsolatedManager(t)
	deleted := make([]string, 0, 1)
	entries := []xiequ.Entry{
		{IP: "203.0.113.10", Memo: "owned-current"},
		{IP: "203.0.113.11", Memo: "owned-stale"},
		{IP: "203.0.113.12", Memo: "manual"},
		{IP: "203.0.113.13", Memo: "manual"},
		{IP: "203.0.113.14", Memo: "manual"},
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "del" {
			deleted = append(deleted, r.URL.Query().Get("ip"))
			entries = append(entries[:1], entries[2:]...)
			_, _ = w.Write([]byte("ok"))
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer upstream.Close()
	config := xiequConfigureFixture(t, m, upstream)
	client := xiequ.NewWithHTTPClient(upstream.URL, "uid-1", "key-1", upstream.Client())
	state := db.XiequWhitelistState{Scope: config.WhitelistScope(), ManagedRecords: []db.XiequManagedRecord{
		{IP: "203.0.113.10", Memo: "owned-current", Scope: config.WhitelistScope()},
		{IP: "203.0.113.11", Memo: "owned-stale", Scope: config.WhitelistScope()},
	}}
	if err := m.saveXiequRefreshState(t.Context(), &state); err != nil {
		t.Fatal(err)
	}
	removed, _, err := m.removeStaleXiequManagedIPs(
		context.Background(),
		client,
		config, &state,
		entries,
		"203.0.113.10",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "203.0.113.11" {
		t.Fatalf("回收结果=%#v，期望只回收托管旧 IP", removed)
	}
	if len(deleted) != 1 || deleted[0] != "203.0.113.11" {
		t.Fatalf("删除请求=%#v，绝不能删除当前出口或手工记录", deleted)
	}
}

func TestXiequCurrentEntryCheckIsExact(t *testing.T) {
	entries := []xiequ.Entry{{IP: "203.0.113.10"}}
	if !entryPresent(entries, "203.0.113.10") {
		t.Fatal("当前出口应被识别为已存在")
	}
	if entryPresent(entries, "203.0.113.1") {
		t.Fatal("IP 匹配必须是完整地址，不能是前缀")
	}
}
