package xiequ

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientRejectsHTTPBeforeSendingCredentials(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = w.Write([]byte(`[]`)) }))
	defer upstream.Close()
	c := NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-secret", upstream.Client())
	if _, err := c.List(context.Background()); err == nil {
		t.Error("HTTP 应在发出凭据前拒绝")
	}
	if requests.Load() != 0 {
		t.Errorf("HTTP 收到 %d 次请求", requests.Load())
	}
}

func TestClientRequiresVerifiedTLSAndRedactsErrors(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = w.Write([]byte(`[]`)) }))
	defer upstream.Close()
	c := New(upstream.URL, "synthetic-uid", "synthetic-secret")
	if _, err := c.List(context.Background()); err == nil || strings.Contains(err.Error(), "synthetic-") {
		t.Fatalf("未受信TLS或错误脱敏失效: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("未受信证书收到凭据")
	}
	c = NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-secret", upstream.Client())
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("受信本地CA失败: %v", err)
	}
	unsafe := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	c = NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-secret", unsafe)
	if _, err := c.List(context.Background()); err == nil {
		t.Error("禁止跳过TLS验证")
	}
	if requests.Load() != 1 {
		t.Fatal("不安全传输不应发出凭据")
	}
}

func TestClientInjectedHTTPClientCannotFollowRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/?uid=synthetic-uid&ukey=synthetic-secret", http.StatusFound)
	}))
	defer upstream.Close()
	c := NewWithHTTPClient(upstream.URL, "synthetic-uid", "synthetic-secret", upstream.Client())
	if _, err := c.List(context.Background()); err == nil {
		t.Error("重定向不能作为成功")
	}
	if leaked.Load() != 0 {
		t.Fatal("重定向泄露凭据")
	}
}
