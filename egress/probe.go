package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProxyURL 验证代理参数，错误不包含原始地址或凭据。
func ProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("代理地址无效")
	}
	return u, nil
}

// ProbeProxy 真正经指定代理访问目标；不读取环境代理，不回退直连。
func ProbeProxy(ctx context.Context, raw, target string, requireWarp bool) error {
	_, err := ProbeProxyIP(ctx, raw, target, requireWarp)
	return err
}

// ProbeProxyIP 复用同一次出站检测返回 trace 中的真实出口 IP，不把代理入口当作出口。
// 自定义检测目标可以不返回 trace，此时出口 IP 留空。
func ProbeProxyIP(ctx context.Context, raw, target string, requireWarp bool) (string, error) {
	u, err := ProxyURL(raw)
	if err != nil {
		return "", err
	}
	if target == "" {
		target = "https://www.cloudflare.com/cdn-cgi/trace"
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u), DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", errors.New("代理检测目标无效")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("代理出站检测失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("代理检测返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024+1))
	if err != nil || len(body) > 16*1024 {
		return "", errors.New("代理检测响应无效")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			values[k] = strings.TrimSpace(v)
		}
	}
	ip := net.ParseIP(values["ip"])
	if requireWarp && (ip == nil || (values["warp"] != "on" && values["warp"] != "plus")) {
		return "", errors.New("代理出口未验证为 WARP")
	}
	if ip != nil {
		return ip.String(), nil
	}
	return "", nil
}
