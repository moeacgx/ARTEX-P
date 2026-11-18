package server

import (
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

func TestXiequQuotaNeverDeletesManagedOrManualIPs(t *testing.T) {
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
		if r.URL.Query().Get("act") != "getjson" {
			deleted = append(deleted, r.URL.Query().Get("ip"))
			_, _ = w.Write([]byte("ok"))
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer upstream.Close()
	config := xiequConfigureFixture(t, m, upstream)
	state := db.XiequWhitelistState{Scope: config.WhitelistScope(), ManagedRecords: []db.XiequManagedRecord{
		{IP: "203.0.113.10", Memo: "owned-current", Scope: config.WhitelistScope()},
		{IP: "203.0.113.11", Memo: "owned-stale", Scope: config.WhitelistScope()},
	}}
	if err := m.saveXiequRefreshState(t.Context(), &state); err != nil {
		t.Fatal(err)
	}
	result, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Reason != "quota_full" || len(result.Removed) != 0 || !strings.Contains(result.Message, "人工处理") {
		t.Fatalf("满额应明确阻断并提示人工处理: %+v", result)
	}
	if len(deleted) != 0 {
		t.Fatalf("满额不能发出任何添加/删除: %#v", deleted)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"removed":[]`) {
		t.Fatalf("removed 契约必须保持空数组: %s", encoded)
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
