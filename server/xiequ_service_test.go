package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/integrations/xiequ"
)

func TestXiequManagedIPHelpers(t *testing.T) {
	entries := []xiequ.Entry{{IP: "203.0.113.7"}, {IP: "203.0.113.8"}}
	managed := appendXiequManagedIP([]string{"203.0.113.7"}, "203.0.113.8")
	if len(managed) != 2 {
		t.Fatalf("追加管理 IP 失败: %#v", managed)
	}
	if got := pruneXiequManagedIPs(managed, entries[:1]); len(got) != 1 || got[0] != "203.0.113.7" {
		t.Fatalf("未清理已消失的管理 IP: %#v", got)
	}
	if got := removeXiequManagedIPs(managed, []string{"203.0.113.7"}); len(got) != 1 || got[0] != "203.0.113.8" {
		t.Fatalf("未移除已回收的管理 IP: %#v", got)
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
	deleted := make([]string, 0, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "del" {
			deleted = append(deleted, r.URL.Query().Get("ip"))
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	client := xiequ.NewWithHTTPClient(upstream.URL, "uid-1", "key-1", upstream.Client())
	entries := []xiequ.Entry{
		{IP: "203.0.113.10"}, // current exit
		{IP: "203.0.113.11"}, // managed stale
		{IP: "203.0.113.12"}, // manual record
	}
	removed, err := (&Manager{}).removeStaleXiequManagedIPs(
		context.Background(),
		client,
		[]string{"203.0.113.10", "203.0.113.11"},
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
