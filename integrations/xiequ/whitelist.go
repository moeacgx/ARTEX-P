package xiequ

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseBytes = 64 << 10
	defaultTimeout   = 20 * time.Second
)

// Entry is one IP whitelist record returned by the Xiequ management API.
type Entry struct {
	IP   string `json:"IP"`
	Memo string `json:"MEMO"`
}

// Client is a deliberately small client for the Xiequ IP whitelist API.
// It never logs or returns the UID/UKEY and does not follow redirects.
type Client struct {
	BaseURL    string
	UID        string
	UKEY       string
	HTTPClient *http.Client
}

func New(baseURL, uid, ukey string) *Client {
	return &Client{BaseURL: baseURL, UID: uid, UKEY: ukey, HTTPClient: DirectHTTPClient()}
}

// NewWithHTTPClient is for tests and trusted callers that already own a
// deliberately configured transport. Production callers should use New.
func NewWithHTTPClient(baseURL, uid, ukey string, client *http.Client) *Client {
	return &Client{BaseURL: baseURL, UID: uid, UKEY: ukey, HTTPClient: client}
}

// DirectHTTPClient explicitly disables environment proxies. The whitelist API
// and public-IP probe must not depend on the proxy they are trying to authorize.
func DirectHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       defaultTimeout,
		Transport:     directTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func directTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: nil}
	}
	tr := base.Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	return tr
}

func (c *Client) List(ctx context.Context) ([]Entry, error) {
	body, err := c.get(ctx, "getjson", nil)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return []Entry{}, nil
	}
	entries, err := decodeEntries(body)
	if err != nil {
		return nil, errors.New("谐趣白名单响应格式无效")
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		ip, ok := publicIPv4(entry.IP)
		if !ok {
			continue
		}
		out = append(out, Entry{IP: ip, Memo: strings.TrimSpace(entry.Memo)})
	}
	return out, nil
}

type entryWire map[string]json.RawMessage

// decodeEntries accepts the response shapes seen across Xiequ deployments:
// a root array, {"data":[...]}, or a nested list object. Field names are
// matched case-insensitively, with "meno" kept as an alias for the documented
// management API spelling.
func decodeEntries(body []byte) ([]Entry, error) {
	var root json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	root = json.RawMessage(strings.TrimSpace(string(root)))
	if len(root) == 0 {
		return []Entry{}, nil
	}
	switch root[0] {
	case '[':
		return decodeEntryArray(root)
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(root, &object); err != nil {
			return nil, err
		}
		for _, key := range []string{"data", "list", "rows"} {
			value, ok := object[key]
			if !ok {
				continue
			}
			value = json.RawMessage(strings.TrimSpace(string(value)))
			if len(value) == 0 || string(value) == "null" {
				return []Entry{}, nil
			}
			if value[0] == '[' {
				return decodeEntryArray(value)
			}
			if value[0] == '{' {
				var nested map[string]json.RawMessage
				if err := json.Unmarshal(value, &nested); err != nil {
					return nil, err
				}
				for _, nestedKey := range []string{"data", "list", "rows"} {
					if nestedValue, ok := nested[nestedKey]; ok {
						nestedValue = json.RawMessage(strings.TrimSpace(string(nestedValue)))
						if len(nestedValue) == 0 || string(nestedValue) == "null" {
							return []Entry{}, nil
						}
						if nestedValue[0] == '[' {
							return decodeEntryArray(nestedValue)
						}
					}
				}
			}
			return nil, errors.New("白名单列表字段不是数组")
		}
		// Some gateways return a single entry object at the root.
		if entry, ok := decodeEntry(object); ok {
			return []Entry{entry}, nil
		}
		return nil, errors.New("白名单列表缺少 data 字段")
	default:
		return nil, errors.New("白名单列表不是 JSON 对象或数组")
	}
}

func decodeEntryArray(raw json.RawMessage) ([]Entry, error) {
	var values []entryWire
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(values))
	for _, value := range values {
		if entry, ok := decodeEntry(value); ok {
			out = append(out, entry)
		}
	}
	return out, nil
}

func decodeEntry(value entryWire) (Entry, bool) {
	ip := entryString(value, "ip")
	if ip == "" {
		return Entry{}, false
	}
	return Entry{
		IP:   ip,
		Memo: entryString(value, "memo", "meno", "remark"),
	}, true
}

func entryString(value entryWire, keys ...string) string {
	for _, key := range keys {
		for actual, raw := range value {
			if !strings.EqualFold(actual, key) {
				continue
			}
			var text string
			if json.Unmarshal(raw, &text) == nil {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func (c *Client) Add(ctx context.Context, ip, memo string) error {
	if !ValidIPv4(ip) {
		return errors.New("白名单 IP 无效")
	}
	memo = strings.TrimSpace(memo)
	if len(memo) > 80 || strings.ContainsFunc(memo, func(r rune) bool { return r == '\r' || r == '\n' || r == '\x00' }) {
		return errors.New("白名单备注无效")
	}
	_, err := c.get(ctx, "add", map[string]string{"ip": ip, "meno": memo})
	return err
}

func (c *Client) Delete(ctx context.Context, ip string) error {
	if strings.EqualFold(strings.TrimSpace(ip), "all") {
		return errors.New("禁止删除全部白名单")
	}
	if !ValidIPv4(ip) {
		return errors.New("白名单 IP 无效")
	}
	_, err := c.get(ctx, "del", map[string]string{"ip": ip})
	return err
}

func (c *Client) get(ctx context.Context, act string, extra map[string]string) ([]byte, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.UID) == "" || strings.TrimSpace(c.UKEY) == "" {
		return nil, errors.New("谐趣白名单配置不完整")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("谐趣白名单接口地址无效")
	}
	q := u.Query()
	q.Set("uid", c.UID)
	q.Set("ukey", c.UKEY)
	q.Set("act", act)
	for key, value := range extra {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("谐趣白名单请求无效")
	}
	req.Header.Set("Accept", "application/json, text/plain")
	req.Header.Set("User-Agent", "ARTEX-P/xiequ-whitelist")
	client := c.HTTPClient
	if client == nil {
		client = DirectHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("谐趣白名单接口连接失败")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("谐趣白名单响应读取失败")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("谐趣白名单响应过大")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("谐趣白名单接口返回 HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// DiscoverPublicIP returns the IPv4 address used by the server for direct
// outbound requests. The same HTTP client settings are used for IP discovery
// and the whitelist API so an environment proxy is represented consistently.
func DiscoverPublicIP(ctx context.Context, client *http.Client) (string, error) {
	if client == nil {
		client = DirectHTTPClient()
	}
	for _, endpoint := range []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
	} {
		body, err := fetchText(ctx, client, endpoint)
		if err != nil {
			continue
		}
		if ip, ok := publicIPv4(string(body)); ok {
			return ip, nil
		}
	}
	body, err := fetchText(ctx, client, "https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return "", errors.New("无法识别当前公网 IPv4 出口")
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "ip" {
			if ip, valid := publicIPv4(value); valid {
				return ip, nil
			}
		}
	}
	return "", errors.New("无法识别当前公网 IPv4 出口")
}

// DiscoverPublicIP probes through this client's configured transport. New
// clients use DirectHTTPClient, so this never routes through the Xiequ proxy.
func (c *Client) DiscoverPublicIP(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("谐趣白名单客户端不可用")
	}
	return DiscoverPublicIP(ctx, c.HTTPClient)
}

func fetchText(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ARTEX-P/xiequ-whitelist")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, errors.New("公网 IP 探测失败")
	}
	return body, nil
}

func ValidIPv4(raw string) bool {
	_, ok := publicIPv4(raw)
	return ok
}

func publicIPv4(raw string) (string, bool) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || ip.To4() == nil {
		return "", false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return "", false
	}
	return ip.String(), true
}
