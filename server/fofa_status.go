package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/db"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 身份仅由已核验工具绑定的服务端装饰器注入，不能从模型参数获取。
type fofaAgentContextKey struct{}

func (s *Server) fofaStatusTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        "fofa_status",
		Description: "读取当前 FOFA 本地配置和当前 Agent 的搜索工具绑定，不联网、不消耗查询额度、不返回令牌。需要确认是否可以使用 FOFA 时调用；历史任务或工具搜索无匹配不能代替当前状态。返回可用仅代表本地条件满足，不代表账户鉴权、额度或出口已验证。",
		Schema:      objSchema(map[string]any{}),
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if ctx.Err() != nil {
				return actool.Errorf("FOFA 状态查询已取消"), nil
			}
			agentKey, trusted := ctx.Value(fofaAgentContextKey{}).(string)
			if !trusted || agentKey == "" {
				return actool.Errorf("FOFA 状态查询缺少可信 Agent 身份"), nil
			}
			if decodeProxyToolInput(input, objSchema(map[string]any{}), nil) != nil {
				return actool.Errorf("FOFA 状态工具不接受参数，请使用空对象调用"), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("FOFA 配置存储不可用，无法判断当前状态"), nil
			}
			config, err := s.m.pg.FOFAConfig()
			if err != nil {
				return actool.Errorf("FOFA 配置读取失败，无法判断当前启用和令牌状态"), nil
			}
			search, err := s.m.pg.GetTool("fofa_search")
			if err != nil {
				return actool.Errorf("FOFA 工具目录读取失败，无法判断当前搜索权限"), nil
			}
			keySet := config.APIKey != ""
			cooldown, err := s.m.pg.FOFARequestCooldown(ctx, config.APIKey)
			pending := errors.Is(err, db.ErrFOFARequestPending)
			if errors.Is(err, db.ErrFOFAThrottleUnsettled) {
				return actool.Errorf(db.ErrFOFAThrottleUnsettled.Error()), nil
			}
			if err != nil && !pending {
				return actool.Errorf("FOFA 冷却状态读取失败，无法判断当前可用性"), nil
			}
			searchEnabled, bound := false, false
			if search != nil {
				searchEnabled = search.Enabled
				bound = contains(search.Agents, agentKey)
			}
			available := config.Enabled && keySet && search != nil && search.System && searchEnabled && bound && cooldown <= 0 && !pending
			reason, message := "available", "当前 Agent 已满足 FOFA 搜索的本地配置与绑定条件，可直接调用 fofa_search"
			switch {
			case search == nil:
				reason, message = "search_tool_missing", "内置 FOFA 搜索工具目录项缺失，请检查程序版本和初始化状态"
			case !search.System:
				reason, message = "search_tool_untrusted", "同名目录项不是内置 FOFA 搜索工具，不能作为搜索可用的依据"
			case !config.Enabled:
				reason, message = "integration_disabled", "FOFA 集成当前未启用，请检查系统设置中的 FOFA 开关"
			case !keySet:
				reason, message = "api_key_missing", "FOFA 当前未保存 API Key，请在系统设置中配置，不要把令牌发送到对话"
			case !searchEnabled:
				reason, message = "search_tool_disabled", "内置 FOFA 搜索工具当前已停用，请检查工具设置"
			case !bound:
				reason, message = "agent_not_bound", "当前 Agent 未绑定 FOFA 搜索工具，请检查 Agent 工具绑定"
			case cooldown > 0:
				reason, message = "rate_limited", "当前 Key 处于所有 Worker 共用的冷却期，请等待 retry_after_seconds 后再查询；更换代理不会解除冷却"
			case pending:
				reason, message = "request_in_progress", "当前 Key 正有请求执行中，请等待结束，不要重复查询"
			}
			// 只返回本地布尔状态，不能将密钥对象或上游认证结论混入输出。
			return jsonResult(map[string]any{
				"integration_enabled":         config.Enabled,
				"api_key_set":                 keySet,
				"search_tool_enabled":         searchEnabled,
				"agent_bound":                 bound,
				"search_tool_available":       available,
				"retry_after_seconds":         fofaRetrySeconds(cooldown),
				"request_interval_seconds":    config.RequestIntervalSeconds,
				"rate_limit_cooldown_seconds": config.RateLimitCooldownSeconds,
				"reason":                      reason,
				"message":                     message + "。本次仅检查本地配置和绑定，未验证 FOFA 账户鉴权、可用额度或网络连通性。",
			})
		},
	})
}
