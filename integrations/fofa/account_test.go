package fofa_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/integrations/fofa"
)

func TestAccountReturnsOnlyNumericQuotaAndPreservesUnknown(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"缺失", `{"error":false}`, `{"fofa_point":null,"remain_free_point":null,"remain_api_query":null,"remain_api_data":null}`},
		{"显式空值", `{"error":false,"fofa_point":null,"remain_free_point":null,"remain_api_query":null,"remain_api_data":null}`, `{"fofa_point":null,"remain_free_point":null,"remain_api_query":null,"remain_api_data":null}`},
		{"真实零", `{"error":false,"fofa_point":0,"remain_free_point":0,"remain_api_query":0,"remain_api_data":0}`, `{"fofa_point":0,"remain_free_point":0,"remain_api_query":0,"remain_api_data":0}`},
		{"部分额度", `{"error":false,"fofa_point":12.5,"remain_api_query":7}`, `{"fofa_point":12.5,"remain_free_point":null,"remain_api_query":7,"remain_api_data":null}`},
		{"完整额度", `{"error":false,"fofa_point":1.25,"remain_free_point":0.5,"remain_api_query":0,"remain_api_data":9007199254740991}`, `{"fofa_point":1.25,"remain_free_point":0.5,"remain_api_query":0,"remain_api_data":9007199254740991}`},
		{"F币不是F点", `{"error":false,"fcoin":999}`, `{"fofa_point":null,"remain_free_point":null,"remain_api_query":null,"remain_api_data":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/api/v1/info/my" || r.URL.Query().Get("key") != "test-account-key" || len(r.URL.Query()) != 1 || r.Method != http.MethodGet {
					t.Error("额度读取必须仅调用账户接口并使用配置令牌")
				}
				fmt.Fprint(w, tc.body)
			})
			account, err := fofa.New(client, "test-account-key").Account(context.Background())
			if err != nil || account == nil {
				t.Fatalf("账户读取失败: account=%v err=%v", account, err)
			}
			encoded, err := json.Marshal(account)
			if err != nil || string(encoded) != tc.want || calls.Load() != 1 {
				t.Fatalf("额度白名单、空值或请求次数错误: got=%s want=%s calls=%d err=%v", encoded, tc.want, calls.Load(), err)
			}
		})
	}
}

func TestAccountDoesNotExposeUnknownPrivateFields(t *testing.T) {
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":false,"fofa_point":9,"email":"private@example.test","username":"private-user","avatar":"https://private.invalid/avatar","key":"test-account-key","fcoin":123,"FOFA_POINT":88,"extra":{"remain_api_data":999,"nested":[{"token":"test-account-key"}]}}`)
	})
	account, err := fofa.New(client, "test-account-key").Account(context.Background())
	if err != nil || account == nil {
		t.Fatalf("账户读取失败: %v", err)
	}
	encoded, _ := json.Marshal(account)
	var values map[string]any
	_ = json.Unmarshal(encoded, &values)
	if !reflect.DeepEqual(values, map[string]any{"fofa_point": float64(9), "remain_free_point": nil, "remain_api_query": nil, "remain_api_data": nil}) {
		t.Fatalf("非白名单或非顶层字段进入账户结果: %s", encoded)
	}
	for _, private := range []string{"test-account-key", "private", "email", "username", "avatar", "fcoin"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("账户结果泄露非白名单信息: %s", encoded)
		}
	}
}

func TestAccountRejectsInvalidQuotaWithoutEcho(t *testing.T) {
	cases := []struct{ field, value string }{}
	for _, field := range []string{"fofa_point", "remain_free_point", "remain_api_query", "remain_api_data"} {
		for _, value := range []string{`"test-account-key"`, `true`, `[]`, `{"secret":"test-account-key"}`, `-1`} {
			cases = append(cases, struct{ field, value string }{field, value})
		}
	}
	for _, field := range []string{"fofa_point", "remain_free_point"} {
		for _, value := range []string{`1e309`, `-0.25`, `-1e-400`, `1e-400`} {
			cases = append(cases, struct{ field, value string }{field, value})
		}
	}
	for _, field := range []string{"remain_api_query", "remain_api_data"} {
		for _, value := range []string{`1.5`, `1.0`, `1e3`, `9007199254740992`, `9007199254740991.1`, `999999999999999999999999999`} {
			cases = append(cases, struct{ field, value string }{field, value})
		}
	}
	for _, tc := range cases {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"error":false,%q:%s,"email":"private@example.test"}`, tc.field, tc.value)
			})
			account, err := fofa.New(client, "test-account-key").Account(context.Background())
			if err == nil || account != nil || !strings.Contains(err.Error(), "无效") || strings.Contains(err.Error(), "test-account-key") || strings.Contains(err.Error(), "private@example") {
				t.Fatalf("无效额度未安全拒绝: account=%v err=%v", account, err)
			}
		})
	}
}

func TestAccountPreservesRateLimitAndAuthenticationErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		limited bool
	}{
		{"HTTP限流", 429, `test-account-key private-upstream-body`, true},
		{"正文限流", 200, `{"error":true,"errmsg":"too many requests test-account-key"}`, true},
		{"鉴权失败", 401, `test-account-key private-upstream-body`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "31")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			account, err := fofa.New(client, "test-account-key").Account(context.Background())
			var limited *fofa.RateLimitError
			if err == nil || account != nil || errors.As(err, &limited) != tc.limited || calls.Load() != 1 {
				t.Fatalf("账户接口未保留失败类型或重复调用: account=%v err=%v calls=%d", account, err, calls.Load())
			}
			if tc.limited && limited.RetryAfter != 31*time.Second {
				t.Fatalf("丢失Retry-After: %v", limited)
			}
			if strings.Contains(err.Error(), "test-account-key") || strings.Contains(err.Error(), "private-upstream-body") {
				t.Fatalf("账户错误泄露敏感内容: %v", err)
			}
		})
	}
}
