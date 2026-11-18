package xiequ

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientListAddDeleteAndParameters(t *testing.T) {
	var calls []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Query().Get("act")+":"+r.URL.Query().Get("ip")+":"+r.URL.Query().Get("meno"))
		if r.URL.Query().Get("uid") != "uid-1" || r.URL.Query().Get("ukey") != "key-1" {
			t.Errorf("鉴权参数未传递")
		}
		switch r.URL.Query().Get("act") {
		case "getjson":
			_, _ = w.Write([]byte(`{"data":[{"IP":"203.0.113.7","MEMO":"manual"}]}`))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()

	client := NewWithHTTPClient(upstream.URL, "uid-1", "key-1", upstream.Client())
	entries, err := client.List(context.Background())
	if err != nil || len(entries) != 1 || entries[0].IP != "203.0.113.7" || entries[0].Memo != "manual" {
		t.Fatalf("List() = %+v, %v", entries, err)
	}
	if err := client.Add(context.Background(), "203.0.113.8", "ARTEX auto"); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), "203.0.113.8"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[1] != "add:203.0.113.8:ARTEX auto" || calls[2] != "del:203.0.113.8:" {
		t.Fatalf("请求参数不正确: %#v", calls)
	}
}

func TestClientListAcceptsRootArrayAndDocumentedMemoAlias(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"ip":"203.0.113.10","meno":"manual"},{"IP":"203.0.113.11","MEMO":"legacy"}]`))
	}))
	defer upstream.Close()

	client := NewWithHTTPClient(upstream.URL, "uid-1", "key-1", upstream.Client())
	entries, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].IP != "203.0.113.10" || entries[0].Memo != "manual" || entries[1].Memo != "legacy" {
		t.Fatalf("兼容列表响应失败: %#v", entries)
	}
}

func TestClientRejectsAllAndDoesNotFollowRedirects(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "del" {
			t.Error("删除 all 不应发出请求")
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer upstream.Close()

	client := NewWithHTTPClient(upstream.URL, "uid-1", "key-1", upstream.Client())
	if err := client.Delete(context.Background(), "all"); err == nil {
		t.Fatal("Delete(all) 应被拒绝")
	}
	if _, err := client.List(context.Background()); err == nil {
		t.Fatal("重定向响应不应被当作成功")
	}
}

func TestDirectHTTPClientDisablesEnvironmentProxy(t *testing.T) {
	client := DirectHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("管理 API 客户端必须显式直连")
	}
}

func TestClientRejectsUnsafeMemo(t *testing.T) {
	client := New("http://127.0.0.1:1", "uid-1", "key-1")
	for _, memo := range []string{"line\nbreak", strings.Repeat("x", 81)} {
		if err := client.Add(context.Background(), "203.0.113.8", memo); err == nil {
			t.Fatalf("不安全备注未被拒绝: %q", memo)
		}
	}
}

func TestClientMutationRequiresExplicitCompleteAcknowledgement(t *testing.T) {
	for _, body := range []string{"", "quota full", "duplicate IP", "<html>ok</html>", `{"success":true}`, "ok"} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer upstream.Close()
			client := NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-key", upstream.Client())
			for _, err := range []error{client.Add(t.Context(), "203.0.113.8", "owned"), client.Delete(t.Context(), "203.0.113.8")} {
				if (err == nil) != (body == "ok") {
					t.Fatalf("正文 %q 被错误分类: %v", body, err)
				}
			}
		})
	}
}

func TestClientEmptyListCannotProveDeletion(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	if _, err := NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-key", upstream.Client()).List(t.Context()); err == nil {
		t.Fatal("空响应不能证明白名单为空或删除完成")
	}
}

func TestClientMalformedListCannotProveDeletion(t *testing.T) {
	for _, body := range []string{`{"data":null}`, `{"data":{"rows":null}}`, `[{}]`, `[{"IP":42}]`, `[{"IP":"bad-ip"}]`, `[{"IP":"203.0.113.99","MEMO":42}]`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer upstream.Close()
			if entries, err := NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-key", upstream.Client()).List(t.Context()); err == nil {
				t.Fatalf("损坏列表被当作完成证据: %+v", entries)
			}
		})
	}
}
