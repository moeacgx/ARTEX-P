package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const proxyQueryTimeout = 5 * time.Second

func isProxyCenterTool(name string) bool {
	return name == "list_proxies" || name == "add_proxies" || name == "test_proxies"
}

func proxyObject(props map[string]any, required ...string) map[string]any {
	s := objSchema(props, required...)
	s["additionalProperties"] = false
	return s
}
func proxyInteger(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func proxySafeIntegerMax() int {
	if strconv.IntSize < 64 {
		return int(^uint(0) >> 1)
	}
	max, _ := strconv.ParseInt("9007199254740991", 10, 64)
	return int(max)
}
func proxyArray(items map[string]any, max int) map[string]any {
	return map[string]any{"type": "array", "items": items, "minItems": 1, "maxItems": max}
}
func proxyAddSchema() map[string]any {
	return proxyObject(map[string]any{"items": proxyArray(proxyObject(map[string]any{
		"item_key":          strParam("每项固定的小写规范 UUID；首次调用前生成并保留，丢失响应或重试必须复用；改配置需新 UUID"),
		"name":              strParam("名称，1 至 80 字节；不是去重键；勿放凭据"),
		"kind":              strParam("http | https | socks5 | resin；不支持 warp"),
		"url":               strParam("普通代理入口 URL，可含认证；固定入口动态出口也使用此字段；不要同时传结构化字段"),
		"protocol":          strParam("Resin 必填 http 或 socks5；普通代理可省略"),
		"host":              strParam("Resin 数据面主机，不含协议或路径"),
		"port":              proxyInteger("Resin 数据面端口", 1, 65535),
		"password":          strParam("Resin 数据面密码；普通代理凭据放入 URL"),
		"resin_platform":    strParam("Resin 平台，默认 Default；字母、数字、下划线、连字符"),
		"resin_admin_url":   strParam("可选 Resin 管理地址，不含凭据和查询串"),
		"resin_admin_token": strParam("可选 Resin 管理令牌"),
		"enabled":           map[string]any{"type": "boolean", "description": "是否启用，默认 true"},
	}, "item_key", "name", "kind"), 50)}, "items")
}
func proxyListSchema() map[string]any {
	return proxyObject(map[string]any{"after_id": proxyInteger("从此 ID 之后分页", 0, proxySafeIntegerMax()), "limit": proxyInteger("返回数量，默认 50", 1, 100)})
}
func proxyTestSchema() map[string]any {
	return proxyObject(map[string]any{
		"proxy_ids":       proxyArray(proxyInteger("代理 ID", 1, proxySafeIntegerMax()), 20),
		"concurrency":     proxyInteger("批内并发，默认 2；进程全局最多 4", 1, 4),
		"timeout_seconds": proxyInteger("单项网络超时，默认 10 秒；整批最多 30 秒，状态写入另限 2 秒", 1, 15),
		"target_url":      strParam("可选 HTTP(S) 检测目标，不含凭据/查询串；默认 Cloudflare trace。仅经指定代理访问，不切换出口"),
	}, "proxy_ids")
}

func (s *Server) proxyCenterTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.proxyCenterTool("list_proxies", "分页查询代理中心，仅返回 ID、名称、类型、状态和脱敏地址。", proxyListSchema(), true, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var a struct {
				AfterID int64 `json:"after_id"`
				Limit   *int  `json:"limit"`
			}
			if decodeProxyToolInput(raw, proxyListSchema(), &a) != nil {
				return actool.Errorf("查询参数无效"), nil
			}
			limit := 50
			if a.Limit != nil {
				limit = *a.Limit
			}
			ctx, cancel := context.WithTimeout(ctx, proxyQueryTimeout)
			defer cancel()
			rows, err := s.m.pg.ListProxyPage(ctx, a.AfterID, limit)
			if err != nil {
				return actool.Errorf("代理查询失败"), nil
			}
			out := make([]*proxyToolView, 0, len(rows))
			for _, p := range rows {
				out = append(out, proxyToolDTO(p))
			}
			next := a.AfterID
			if len(rows) > 0 {
				next = rows[len(rows)-1].ID
			}
			return jsonResult(map[string]any{"proxies": out, "next_after_id": next})
		}),
		s.proxyCenterTool("add_proxies", "批量登记已有 HTTP/HTTPS/SOCKS5 或结构化 Resin 入口，支持固定入口动态出口；不联系供应商、不刷新订阅、不接受条款、不创建 WARP。每项 item_key 必须持久复用，逐项返回成功/失败；同 key 异载荷冲突，已删除不会重建。输入凭据可能已进入对话、LLM 请求或录制；存储加密与回显脱敏不代表输入无痕。", proxyAddSchema(), false, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var a proxyAddInput
			if decodeProxyToolInput(raw, proxyAddSchema(), &a) != nil {
				return actool.Errorf("批量添加参数无效"), nil
			}
			return jsonResult(map[string]any{"items": s.addProxyItems(ctx, a)})
		}),
		s.proxyCenterTool("test_proxies", "经指定档案联网检测并更新健康状态；这是网络和数据库写操作。失败不回退到其他出口或直连，不修改当前任务出口；不支持 WARP。", proxyTestSchema(), false, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var a proxyTestInput
			if decodeProxyToolInput(raw, proxyTestSchema(), &a) != nil || !validProxyProbeTarget(a.TargetURL) {
				return actool.Errorf("检测参数无效"), nil
			}
			seen := map[int64]bool{}
			for _, id := range a.ProxyIDs {
				if seen[id] {
					return actool.Errorf("proxy_ids 不能重复"), nil
				}
				seen[id] = true
			}
			return jsonResult(map[string]any{"items": s.testProxyItems(ctx, a)})
		}),
	}
}

type proxyToolAgentKey struct{}

func (s *Server) proxyCenterTool(name, desc string, schema map[string]any, readOnly bool, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{Name: name, Description: desc, Schema: schema, RawInput: true,
		ReadOnly: func(json.RawMessage) bool { return readOnly }, Concurrent: func(json.RawMessage) bool { return readOnly },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Denied("代理工具需通过 Agent 绑定授权")
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if key, _ := ctx.Value(proxyToolAgentKey{}).(string); key == "" {
				return actool.Errorf("代理工具未授权"), nil
			}
			return run(ctx, raw)
		},
	})
}

// 绑定由 ToolResolve 注入可信 Agent 身份；每次调用重查授权，防止会话缓存绕过解绑。
type authorizedProxyTool struct {
	actool.CoreTool
	pg       *db.DB
	agentKey string
	schema   map[string]any
}

func (t *authorizedProxyTool) CheckPermissions(ctx context.Context, _ json.RawMessage, _ permission.Context) permission.Decision {
	ctx, cancel := context.WithTimeout(ctx, proxyQueryTimeout)
	defer cancel()
	var allowed bool
	err := t.pg.QueryRowContext(ctx, `SELECT enabled AND system AND agents ? $2 FROM tools WHERE key=$1`, t.Name(), t.agentKey).Scan(&allowed)
	if err != nil || !allowed {
		return permission.Denied("代理工具已禁用、解绑或授权不可用")
	}
	return permission.Allowed()
}
func (t *authorizedProxyTool) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if t.CheckPermissions(ctx, raw, permission.Context{}).Behavior != permission.Allow {
		return actool.Errorf("代理工具未授权"), nil
	}
	// 在通用默认值装饰器前检查，避免重复键/null 被它的重新编码消掉。
	if decodeProxyToolInput(raw, t.schema, nil) != nil {
		return actool.Errorf("代理工具参数无效"), nil
	}
	return t.CoreTool.Call(context.WithValue(ctx, proxyToolAgentKey{}, t.agentKey), raw, tc)
}

// 严格解码拒绝未知字段、大小写别名、重复键、null、尾随值和深层嵌套。
// 不把标准库错误返回给模型，因为其中可能包含原始输入。
func decodeProxyToolInput(raw []byte, schema map[string]any, dst any) error {
	bad := errors.New("代理参数无效")
	if len(raw) == 0 || len(raw) > proxyToolInputLimit {
		return bad
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	value, err := readProxyJSON(dec, 0)
	if err != nil {
		return bad
	}
	if _, err = dec.Token(); err != io.EOF {
		return bad
	}
	if !validateProxyJSON(value, schema) {
		return bad
	}
	if dst != nil && json.Unmarshal(raw, dst) != nil {
		return bad
	}
	return nil
}
func readProxyJSON(dec *json.Decoder, depth int) (any, error) {
	if depth > 12 {
		return nil, errors.New("嵌套过深")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		result := map[string]any{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			str, ok := key.(string)
			if !ok {
				return nil, errors.New("无效键")
			}
			if _, exists := result[str]; exists {
				return nil, errors.New("重复键")
			}
			v, err := readProxyJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			result[str] = v
		}
		_, err = dec.Token()
		return result, err
	case '[':
		result := []any{}
		for dec.More() {
			v, err := readProxyJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, v)
		}
		_, err = dec.Token()
		return result, err
	default:
		return nil, errors.New("无效结构")
	}
}
func validateProxyJSON(v any, s map[string]any) bool {
	switch s["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		props, ok := s["properties"].(map[string]any)
		if !ok {
			return false
		}
		for key, value := range m {
			prop, ok := props[key].(map[string]any)
			if !ok || !validateProxyJSON(value, prop) {
				return false
			}
		}
		if raw, exists := s["required"]; exists {
			required, ok := raw.([]any)
			if !ok {
				return false
			}
			for _, k := range required {
				key, ok := k.(string)
				if !ok {
					return false
				}
				if _, ok := m[key]; !ok {
					return false
				}
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok || !proxyJSONWithinBounds(int64(len(a)), s, "minItems", "maxItems") {
			return false
		}
		items, ok := s["items"].(map[string]any)
		if !ok {
			return false
		}
		for _, item := range a {
			if !validateProxyJSON(item, items) {
				return false
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		val, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || !proxyJSONWithinBounds(val, s, "minimum", "maximum") {
			return false
		}
	case "string":
		if _, ok := v.(string); !ok {
			return false
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return false
		}
	default:
		return false
	}
	return true
}

// 内置定义允许省略可选边界；错误类型必须拒绝，不能让参数校验触发崩溃。
func proxyJSONWithinBounds(value int64, schema map[string]any, minKey, maxKey string) bool {
	if raw, exists := schema[minKey]; exists {
		bound, ok := raw.(int)
		if !ok || value < int64(bound) {
			return false
		}
	}
	if raw, exists := schema[maxKey]; exists {
		bound, ok := raw.(int)
		if !ok || value > int64(bound) {
			return false
		}
	}
	return true
}
