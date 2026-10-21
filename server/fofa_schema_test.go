package server

import (
	"encoding/json"
	"testing"
)

// 复现实际请求中的合法字段数组；校验失败必须返回错误而不是崩溃。
func TestFOFASchemaAcceptsExplicitFields(t *testing.T) {
	raw := []byte(`{"query":"body=\"dujiaoka\"","page":1,"size":50,"fields":["host","ip","port","protocol","domain","title","server","country_name"]}`)
	if err := decodeProxyToolInput(raw, fofaSchema(), nil); err != nil {
		t.Fatalf("合法 fields 被拒绝: %v", err)
	}
}

func TestProxyJSONOptionalConstraints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		schema map[string]any
		want   bool
	}{
		{"数组无边界", []any{"host"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, true},
		{"空数组无边界", []any{}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, true},
		{"数组边界类型错误", []any{"host"}, map[string]any{"type": "array", "minItems": "0", "items": map[string]any{"type": "string"}}, false},
		{"数组元素定义缺失", []any{"host"}, map[string]any{"type": "array"}, false},
		{"整数无边界", json.Number("1"), map[string]any{"type": "integer"}, true},
		{"整数单边界", json.Number("2"), map[string]any{"type": "integer", "minimum": 1}, true},
		{"整数超出边界", json.Number("2"), map[string]any{"type": "integer", "maximum": 1}, false},
		{"整数边界类型错误", json.Number("1"), map[string]any{"type": "integer", "minimum": "0"}, false},
		{"属性定义错误", map[string]any{"a": "b"}, map[string]any{"type": "object", "properties": "invalid"}, false},
		{"必填字段类型错误", map[string]any{}, map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{1}}, false},
		{"必填列表类型错误", map[string]any{}, map[string]any{"type": "object", "properties": map[string]any{}, "required": "a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateProxyJSON(tc.value, tc.schema); got != tc.want {
				t.Fatalf("校验=%v，期望=%v", got, tc.want)
			}
		})
	}
}
