package fofa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase          = "https://fofa.info/api/v1"
	maxResponseBytes = 2 * 1024 * 1024
	requestTimeout   = 30 * time.Second
)

var allowedFields = map[string]bool{
	"host": true, "ip": true, "port": true, "protocol": true,
	"domain": true, "title": true, "server": true, "country": true,
	"country_name": true, "region": true, "city": true, "as_number": true,
	"as_organization": true, "icp": true,
}

// Query 只允许查询和结果范围；地址和凭据由服务端提供。
type Query struct {
	Query  string   `json:"query"`
	Page   int      `json:"page"`
	Size   int      `json:"size"`
	Fields []string `json:"fields"`
}

// SearchResult 将上游按列数组转换为带字段名的记录，不自动请求下一页。
type SearchResult struct {
	Query   string           `json:"query"`
	Page    int              `json:"page"`
	Size    int              `json:"size"`
	Total   int64            `json:"total"`
	Fields  []string         `json:"fields"`
	Results []map[string]any `json:"results"`
	HasMore bool             `json:"has_more"`
}

type Client struct {
	httpClient *http.Client
	key        string
}

// New 复用调用方传输层，使代理策略保持一致；不修改传入客户端。
func New(client *http.Client, key string) *Client {
	configured := http.Client{}
	if client != nil {
		configured = *client
	}
	if configured.Timeout <= 0 || configured.Timeout > requestTimeout {
		configured.Timeout = requestTimeout
	}
	// API Key 位于查询参数中，任何重定向都不能携带它离开固定入口。
	configured.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{httpClient: &configured, key: strings.TrimSpace(key)}
}

func (c *Client) Search(ctx context.Context, q Query) (*SearchResult, error) {
	q, err := normalizeQuery(q)
	if err != nil {
		return nil, err
	}
	values := url.Values{
		"qbase64": {base64.StdEncoding.EncodeToString([]byte(q.Query))},
		"page":    {strconv.Itoa(q.Page)},
		"size":    {strconv.Itoa(q.Size)},
		"fields":  {strings.Join(q.Fields, ",")},
	}
	data, err := c.get(ctx, "/search/all", values)
	if err != nil {
		return nil, err
	}
	var response struct {
		Page *int `json:"page"`
		// FOFA 的 size 表示命中总数，并非本次请求的每页数量。
		Size    *int64  `json:"size"`
		Results [][]any `json:"results"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Page == nil || *response.Page != q.Page || response.Size == nil || *response.Size < 0 || response.Results == nil {
		return nil, malformedResponse()
	}
	offset := int64((q.Page - 1) * q.Size)
	remaining := max(*response.Size-offset, 0)
	if len(response.Results) > q.Size || int64(len(response.Results)) > remaining {
		return nil, malformedResponse()
	}
	result := &SearchResult{
		Query: c.redact(q.Query), Page: q.Page, Size: q.Size, Total: *response.Size,
		Fields: q.Fields, Results: make([]map[string]any, 0, len(response.Results)),
		HasMore: q.Page < 100 && int64(q.Page*q.Size) < *response.Size,
	}
	for _, row := range response.Results {
		if len(row) != len(q.Fields) {
			return nil, malformedResponse()
		}
		record := make(map[string]any, len(q.Fields))
		for i, field := range q.Fields {
			record[field] = c.redactValue(row[i])
		}
		result.Results = append(result.Results, record)
	}
	return result, nil
}

// Test 只验证账户接口，不搜索资产、不返回账户隐私信息。
func (c *Client) Test(ctx context.Context) error {
	_, err := c.get(ctx, "/info/my", url.Values{})
	return err
}

// ValidateQuery 在申请请求配额前验证参数，不联网，也不修改调用方的查询。
func ValidateQuery(q Query) error {
	_, err := normalizeQuery(q)
	return err
}

func normalizeQuery(q Query) (Query, error) {
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" || len(q.Query) > 4096 {
		return Query{}, errors.New("FOFA 查询不能为空，且长度不能超过 4096 字节")
	}
	if q.Page == 0 {
		q.Page = 1
	}
	if q.Size == 0 {
		q.Size = 20
	}
	if q.Page < 1 || q.Page > 100 || q.Size < 1 || q.Size > 100 {
		return Query{}, errors.New("FOFA 页码和每页数量均须在 1 至 100 之间")
	}
	if len(q.Fields) == 0 {
		q.Fields = []string{"host", "ip", "port", "protocol", "title"}
	} else {
		// 不保留调用方切片，防止校验后字段与返回结果失配。
		q.Fields = append([]string(nil), q.Fields...)
	}
	seen := make(map[string]bool, len(q.Fields))
	for i, field := range q.Fields {
		field = strings.TrimSpace(field)
		if !allowedFields[field] || seen[field] {
			return Query{}, errors.New("FOFA 返回字段不受支持或存在重复")
		}
		seen[field] = true
		q.Fields[i] = field
	}
	return q, nil
}

func (c *Client) get(ctx context.Context, path string, values url.Values) ([]byte, error) {
	if c == nil || c.key == "" {
		return nil, errors.New("FOFA API Key 未配置")
	}
	values.Set("key", c.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path+"?"+values.Encode(), nil)
	if err != nil {
		return nil, errors.New("FOFA 请求初始化失败")
	}
	req.Header.Set("Accept", "application/json")
	// 配额请求不做应用层自动重试，失败由使用者决定是否再次查询。
	response, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("FOFA 请求已取消: %w", context.Canceled)
		}
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
			return nil, fmt.Errorf("FOFA 请求超时: %w", context.DeadlineExceeded)
		}
		// url.Error 含完整请求地址和令牌，不能向外包装或输出。
		return nil, errors.New("FOFA 网络请求失败，请检查代理和网络连接")
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, errors.New("FOFA 鉴权失败，请检查 API Key 和账户权限")
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, &RateLimitError{RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now())}
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return nil, errors.New("FOFA 服务返回重定向，已拒绝跟随")
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, fmt.Errorf("FOFA 服务请求失败（HTTP %d）", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("FOFA 请求已取消: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("FOFA 请求超时: %w", context.DeadlineExceeded)
		}
		return nil, errors.New("FOFA 响应读取失败")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("FOFA 响应超过 2 MiB 限制，请缩小结果数量或返回字段")
	}
	var envelope struct {
		Error   *bool  `json:"error"`
		Message string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Error == nil {
		return nil, malformedResponse()
	}
	if *envelope.Error {
		return nil, rejectedResponse(envelope.Message, parseRetryAfter(response.Header.Get("Retry-After"), time.Now()))
	}
	return data, nil
}

func malformedResponse() error {
	return errors.New("FOFA 返回了无效或不完整的结果")
}

func rejectedResponse(message string, retryAfter time.Duration) error {
	message = strings.ToLower(message)
	// 仅用于分类，绝不转发上游正文或原始错误消息。
	for _, word := range []string{"quota", "insufficient", "credits", "余额", "额度", "积分", "f点"} {
		if strings.Contains(message, word) {
			return errors.New("FOFA 账户额度不足，请检查可用查询额度")
		}
	}
	for _, word := range []string{"rate limit", "too many requests", "限流", "频率"} {
		if strings.Contains(message, word) {
			return &RateLimitError{RetryAfter: retryAfter}
		}
	}
	for _, word := range []string{"api key", "api_key", "apikey", "unauthorized", "forbidden", "鉴权", "认证"} {
		if strings.Contains(message, word) {
			return errors.New("FOFA 鉴权失败，请检查 API Key 和账户权限")
		}
	}
	return errors.New("FOFA 拒绝请求，请检查查询语法、账户权限或可用额度")
}

func (c *Client) redact(value string) string {
	for _, secret := range []string{c.key, url.QueryEscape(c.key), url.PathEscape(c.key)} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func (c *Client) redactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return c.redact(typed)
	case []any:
		for i, item := range typed {
			typed[i] = c.redactValue(item)
		}
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, item := range typed {
			cleaned[c.redact(key)] = c.redactValue(item)
		}
		return cleaned
	}
	return value
}
