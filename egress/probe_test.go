package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyProbeReturnsObservedIPOnce(t *testing.T) {
	for _, observed := range []string{"8.8.8.8", "2606:4700:4700::1111", "invalid", ""} {
		t.Run(observed, func(t *testing.T) {
			var requests atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				fmt.Fprintf(w, "ip=%s\nwarp=off\n", observed)
			}))
			defer proxy.Close()
			ip, err := ProbeProxyIP(t.Context(), proxy.URL, "http://unresolvable.invalid/trace", false)
			want := observed
			if net.ParseIP(observed) == nil {
				want = ""
			}
			if err != nil || ip != want || requests.Load() != 1 {
				t.Fatalf("出口=%q 预期=%q 请求=%d 错误=%v", ip, want, requests.Load(), err)
			}
		})
	}
}

func TestProxyProbeCancellationIsBounded(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	ip, err := ProbeProxyIP(ctx, proxy.URL, "http://unresolvable.invalid/trace", false)
	if err == nil || ip != "" || time.Since(start) > time.Second {
		t.Fatalf("取消未及时失败: 出口=%q 错误=%v", ip, err)
	}
}

func TestProxyProbeHTTPAndWarpAreReal(t *testing.T) {
	var requests atomic.Int64
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "ip=192.0.2.1\nwarp=on\n")
	}))
	defer p.Close()
	for _, warp := range []bool{false, true} {
		if err := ProbeProxy(t.Context(), p.URL, "http://unresolvable.invalid/trace", warp); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("没有真正访问代理")
	}
	p.Close()
	if err := ProbeProxy(context.Background(), p.URL, "http://unresolvable.invalid/trace", false); err == nil {
		t.Fatal("关闭代理后仍成功")
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("失败请求泄漏直连") }))
	defer direct.Close()
	if err := ProbeProxy(t.Context(), "http://user:secret@127.0.0.1:1", direct.URL, false); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("失败未闭锁或错误泄漏凭据")
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ip=192.0.2.1\nwarp=off\n") }))
	defer plain.Close()
	if ProbeProxy(t.Context(), plain.URL, "http://example.invalid/", true) == nil {
		t.Fatal("非 WARP 被标记为 WARP")
	}
}

// 本地 SOCKS5 服务执行真实握手与 CONNECT，不依赖外部网络。
func TestProxyProbeSOCKS5(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		var greeting [2]byte
		if _, err = io.ReadFull(c, greeting[:]); err != nil {
			done <- err
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, err = io.ReadFull(c, methods); err != nil {
			done <- err
			return
		}
		c.Write([]byte{5, 0})
		var request [4]byte
		if _, err = io.ReadFull(c, request[:]); err != nil {
			done <- err
			return
		}
		length := 0
		switch request[3] {
		case 1:
			length = 4
		case 4:
			length = 16
		case 3:
			var n [1]byte
			io.ReadFull(c, n[:])
			length = int(n[0])
		default:
			done <- fmt.Errorf("地址类型无效")
			return
		}
		address := make([]byte, length+2)
		if _, err = io.ReadFull(c, address); err != nil {
			done <- err
			return
		}
		up, err := net.Dial("tcp", u.Host)
		if err != nil {
			done <- err
			return
		}
		defer up.Close()
		c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
		go func() { io.Copy(up, c); up.Close() }()
		_, err = io.Copy(c, up)
		done <- err
	}()
	if err = ProbeProxy(t.Context(), "socks5://"+listener.Addr().String(), target.URL, false); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
