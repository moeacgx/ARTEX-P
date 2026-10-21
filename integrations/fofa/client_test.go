package fofa_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/integrations/fofa"
)

// 保持生产请求的域名和路径，只把受控测试的拨号目标改为本地 TLS 服务。
func localClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	address := strings.TrimPrefix(server.URL, "https://")
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func TestSearchDefaultsEncodingAndStructuredResult(t *testing.T) {
	var calls atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "fofa.info" || r.URL.Path != "/api/v1/search/all" || r.Method != http.MethodGet {
			t.Errorf("非预期的请求目的地: %s %s%s", r.Method, r.Host, r.URL.Path)
		}
		values := r.URL.Query()
		query, err := base64.StdEncoding.DecodeString(values.Get("qbase64"))
		if err != nil || string(query) != `title="测试" && country="CN"` {
			t.Errorf("查询未使用标准 base64 编码: %q, %v", query, err)
		}
		if values.Get("key") != "test-key" || values.Get("page") != "1" || values.Get("size") != "20" || values.Get("fields") != "host,ip,port,protocol,title" || values.Get("email") != "" {
			t.Error("默认查询参数错误")
		}
		fmt.Fprint(w, `{"error":false,"query":"ignored","page":1,"size":23,"results":[["https://example.test","192.0.2.1","443","https","测试"]]}`)
	})
	result, err := fofa.New(client, "test-key").Search(context.Background(), fofa.Query{Query: `title="测试" && country="CN"`})
	if err != nil || result == nil {
		t.Fatalf("搜索应返回结构化结果: result=%v err=%v", result, err)
	}
	if result.Page != 1 || result.Size != 20 || result.Total != 23 || !result.HasMore || len(result.Results) != 1 {
		t.Fatalf("分页结果错误: %+v", result)
	}
	if result.Results[0]["ip"] != "192.0.2.1" || result.Results[0]["title"] != "测试" || calls.Load() != 1 {
		t.Fatalf("结果映射或请求次数错误: %+v calls=%d", result.Results, calls.Load())
	}
}

func TestSearchValidationDoesNotSpendRequests(t *testing.T) {
	var calls atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	cases := []struct {
		name  string
		key   string
		query fofa.Query
	}{
		{"未配置令牌", "  ", fofa.Query{Query: "test"}},
		{"空查询", "key", fofa.Query{Query: " \n "}},
		{"过长查询", "key", fofa.Query{Query: strings.Repeat("x", 4097)}},
		{"负页码", "key", fofa.Query{Query: "test", Page: -1}},
		{"超大页码", "key", fofa.Query{Query: "test", Page: 101}},
		{"负数量", "key", fofa.Query{Query: "test", Size: -1}},
		{"超大数量", "key", fofa.Query{Query: "test", Size: 101}},
		{"正文禁用", "key", fofa.Query{Query: "test", Fields: []string{"body"}}},
		{"空字段", "key", fofa.Query{Query: "test", Fields: []string{""}}},
		{"重复字段", "key", fofa.Query{Query: "test", Fields: []string{"host", "host"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fofa.New(client, tc.key).Search(context.Background(), tc.query); err == nil {
				t.Fatal("无效参数应在发起请求之前被拒绝")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("无效参数消耗了 %d 次请求", calls.Load())
	}
}

func TestSearchRequestedFieldsPaginationAndSecretRedaction(t *testing.T) {
	key := "secret-FOFA-api-key"
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fields") != "title,port" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("size") != "2" {
			t.Error("自定义分页或字段未传递")
		}
		json.NewEncoder(w).Encode(map[string]any{"error": false, "page": 2, "size": 3, "results": [][]any{{"echo " + key, 443}}})
	})
	fields := []string{"title", "port"}
	result, err := fofa.New(client, key).Search(context.Background(), fofa.Query{Query: "title=" + key, Page: 2, Size: 2, Fields: fields})
	if err != nil || result == nil {
		t.Fatalf("搜索失败: %v", err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), key) || result.HasMore || !reflect.DeepEqual(result.Fields, fields) || result.Results[0]["port"] != float64(443) {
		t.Fatalf("脱敏或分页错误: %s", data)
	}
}

func TestSearchRejectsMalformedAndOversizedResponses(t *testing.T) {
	cases := []struct{ name, body string }{
		{"空对象", `{}`},
		{"截断JSON", `{"error":false,"page":1,"size":1,"results":[`},
		{"尾部JSON", `{"error":false,"page":1,"size":0,"results":[]} {}`},
		{"缺少总数", `{"error":false,"page":1,"results":[]}`},
		{"缺少结果", `{"error":false,"page":1,"size":0}`},
		{"错页结果", `{"error":false,"page":2,"size":0,"results":[]}`},
		{"负数总数", `{"error":false,"page":1,"size":-1,"results":[]}`},
		{"缺少列", `{"error":false,"page":1,"size":1,"results":[["host"]]}`},
		{"多余列", `{"error":false,"page":1,"size":1,"results":[["a","b","c","d","e","f"]]}`},
		{"结果超量", `{"error":false,"page":1,"size":0,"results":[["a","b","c","d","e"]]}`},
		{"响应过大", `{"error":false,"page":1,"size":1,"results":[["` + strings.Repeat("x", 2*1024*1024) + `","b","c","d","e"]]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			result, err := fofa.New(client, "test-key").Search(context.Background(), fofa.Query{Query: "test"})
			if err == nil || result != nil {
				t.Fatalf("不完整结果不能作为成功返回: result=%v err=%v", result, err)
			}
		})
	}
}

func TestSearchEmptyResultRemainsJSONArray(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":false,"page":1,"size":0,"results":[]}`)
	})
	result, err := fofa.New(client, "test-key").Search(context.Background(), fofa.Query{Query: "test"})
	if err != nil || result == nil {
		t.Fatalf("空结果应成功: %v", err)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"results":[]`) || result.HasMore {
		t.Fatalf("空结果格式错误: %s", data)
	}
}

func TestSearchRejectsMoreRowsThanRequested(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":false,"page":1,"size":10,"results":[["a"],["b"]]}`)
	})
	if result, err := fofa.New(client, "test-key").Search(context.Background(), fofa.Query{Query: "test", Size: 1, Fields: []string{"host"}}); err == nil || result != nil {
		t.Fatalf("返回数量不能超过请求上限: result=%v err=%v", result, err)
	}
}

func TestSearchLastAllowedPageHasNoMore(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":false,"page":100,"size":100000,"results":[["example.test"]]}`)
	})
	result, err := fofa.New(client, "test-key").Search(context.Background(), fofa.Query{Query: "test", Page: 100, Size: 100, Fields: []string{"host"}})
	if err != nil || result == nil || result.HasMore || result.Total != 100000 {
		t.Fatalf("最后允许页不能引导继续查询: result=%v err=%v", result, err)
	}
}

func TestSearchRedactsNestedAndEncodedSecrets(t *testing.T) {
	key := "secret+FOFA/key"
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": false, "page": 1, "size": 1, "results": [][]any{{map[string]any{key: []any{key, url.QueryEscape(key), url.PathEscape(key)}}}}})
	})
	result, err := fofa.New(client, key).Search(context.Background(), fofa.Query{Query: "test", Fields: []string{"title"}})
	if err != nil || result == nil {
		t.Fatalf("搜索失败: %v", err)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{key, url.QueryEscape(key), url.PathEscape(key)} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("嵌套结果泄露了令牌")
		}
	}
}

func TestAPIErrorsAreSanitizedAndNeverRetried(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body, message string
	}{
		{"鉴权", 401, `secret-test-key request-url private-upstream-detail`, "鉴权"},
		{"权限", 403, `secret-test-key private-upstream-detail`, "鉴权"},
		{"限流", 429, `secret-test-key private-upstream-detail`, "限流"},
		{"服务错误", 503, `secret-test-key private-upstream-detail`, "服务"},
		{"200报错", 200, `{"error":true,"errmsg":"private-upstream-detail secret-test-key"}`, "拒绝"},
		{"额度不足", 200, `{"error":true,"errmsg":"insufficient quota private-upstream-detail secret-test-key"}`, "额度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := fofa.New(client, "secret-test-key").Search(context.Background(), fofa.Query{Query: "test"})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("未返回明确错误: %v", err)
			}
			if strings.Contains(err.Error(), "secret-test-key") || strings.Contains(err.Error(), "private-upstream-detail") || strings.Contains(err.Error(), "https://") {
				t.Fatalf("错误泄露上游信息: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("额度请求被自动重试: %d", calls.Load())
			}
		})
	}
}

func TestRedirectIsRejectedAndCallerClientIsNotMutated(t *testing.T) {
	var calls atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://evil.invalid/leak", http.StatusFound)
	})
	client.Timeout = time.Minute
	var originalRedirect atomic.Int32
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { originalRedirect.Add(1); return nil }
	_, err := fofa.New(client, "secret-test-key").Search(context.Background(), fofa.Query{Query: "test"})
	if err == nil || calls.Load() != 1 || originalRedirect.Load() != 0 {
		t.Fatalf("重定向必须被拒绝: %v calls=%d redirects=%d", err, calls.Load(), originalRedirect.Load())
	}
	if client.Timeout != time.Minute {
		t.Fatal("调用方超时配置被修改")
	}
	if err := client.CheckRedirect(nil, nil); err != nil || originalRedirect.Load() != 1 {
		t.Fatal("调用方重定向配置被修改")
	}
}

func TestCancellationAndTimeoutStaySanitized(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fofa.New(client, "secret-test-key").Search(ctx, fofa.Query{Query: "test"})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("取消应保留语义并脱敏: %v", err)
	}
	client.Timeout = 25 * time.Millisecond
	_, err = fofa.New(client, "secret-test-key").Search(context.Background(), fofa.Query{Query: "test"})
	if err == nil || !strings.Contains(err.Error(), "超时") || strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("超时未正确处理: %v", err)
	}
}

func TestResponseBodyTimeoutStaysSanitized(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	client.Timeout = 25 * time.Millisecond
	_, err := fofa.New(client, "secret-test-key").Search(context.Background(), fofa.Query{Query: "test"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "超时") || strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("读取响应超时未脱敏或未保留取消语义: %v", err)
	}
}

func TestConnectionUsesAccountEndpointAndDoesNotExposeAccountData(t *testing.T) {
	var calls atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/info/my" || r.URL.Query().Get("key") != "test-key" || len(r.URL.Query()) != 1 {
			t.Error("连接测试必须使用账户端点")
		}
		fmt.Fprint(w, `{"error":false,"email":"private@example.test","username":"private-user"}`)
	})
	if err := fofa.New(client, "test-key").Test(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("连接测试失败: %v calls=%d", err, calls.Load())
	}
	if err := fofa.New(client, "").Test(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatal("未配置令牌不应发送请求")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("private-upstream-detail %s", r.URL.String())
}

func TestTransportErrorsDoNotExposeURLOrKey(t *testing.T) {
	client := &http.Client{Transport: failingTransport{}}
	_, err := fofa.New(client, "secret-test-key").Search(context.Background(), fofa.Query{Query: "test"})
	if err == nil || strings.Contains(err.Error(), "secret-test-key") || strings.Contains(err.Error(), "private-upstream-detail") || strings.Contains(err.Error(), url.QueryEscape("secret-test-key")) {
		t.Fatalf("网络错误泄露了请求: %v", err)
	}
}
