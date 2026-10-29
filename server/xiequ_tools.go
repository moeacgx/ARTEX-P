package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type xiequAgentContextKey struct{}

func xiequSchema() map[string]any {
	return objSchema(map[string]any{})
}

func (s *Server) xiequStatusTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        "xiequ_whitelist_status",
		Description: "读取谐趣代理池白名单的本地配置、当前 Agent 绑定和最近一次刷新状态。不联网、不修改白名单、不返回 UKEY。需要确认能否使用谐趣代理或排查白名单问题时调用；账户配额上限为 5 个，只有按次/流量计费产品可按客服规则申请增加，包时/包日/包月产品不能增加。",
		Schema:      xiequSchema(),
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if ctx.Err() != nil {
				return actool.Errorf("谐趣白名单状态查询已取消"), nil
			}
			agentKey, trusted := ctx.Value(xiequAgentContextKey{}).(string)
			if !trusted || agentKey == "" {
				return actool.Errorf("谐趣白名单状态查询缺少可信 Agent 身份"), nil
			}
			if decodeProxyToolInput(input, xiequSchema(), nil) != nil {
				return actool.Errorf("谐趣白名单状态工具不接受参数，请使用空对象调用"), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("谐趣白名单配置存储不可用，无法判断当前状态"), nil
			}
			config, err := s.m.pg.XiequConfig()
			if err != nil {
				return actool.Errorf("谐趣白名单配置读取失败，请检查系统设置"), nil
			}
			row, err := s.m.pg.GetTool("xiequ_whitelist_refresh")
			if err != nil {
				return actool.Errorf("谐趣白名单工具目录读取失败，无法判断当前权限"), nil
			}
			refreshEnabled, bound := false, false
			if row != nil {
				refreshEnabled = row.Enabled
				bound = contains(row.Agents, agentKey)
			}
			configured := config.UID != "" && config.UKEY != ""
			available := config.Enabled && configured && row != nil && row.System && refreshEnabled && bound
			reason, message := "available", "当前 Agent 已满足谐趣白名单刷新工具的本地配置与绑定条件"
			switch {
			case row == nil:
				reason, message = "refresh_tool_missing", "内置谐趣白名单刷新工具目录项缺失，请检查程序版本和初始化状态"
			case !row.System:
				reason, message = "refresh_tool_untrusted", "同名目录项不是内置谐趣白名单刷新工具，不能作为可用依据"
			case !config.Enabled:
				reason, message = "integration_disabled", "谐趣白名单自动刷新未启用，请检查系统设置"
			case !configured:
				reason, message = "credentials_missing", "谐趣 UID 或 UKEY 未配置，请检查系统设置"
			case !refreshEnabled:
				reason, message = "refresh_tool_disabled", "谐趣白名单刷新工具当前已停用，请检查工具设置"
			case !bound:
				reason, message = "agent_not_bound", "当前 Agent 未绑定谐趣白名单刷新工具，请检查 Agent 工具绑定"
			}
			out := map[string]any{
				"integration_enabled":      config.Enabled,
				"configured":               configured,
				"uid_set":                  config.UID != "",
				"ukey_set":                 config.UKEY != "",
				"base_url":                 config.BaseURL,
				"memo":                     config.Memo,
				"refresh_interval_seconds": config.RefreshIntervalSeconds,
				"proxy_domains":            config.ProxyDomains,
				"refresh_tool_enabled":     refreshEnabled,
				"agent_bound":              bound,
				"available":                available,
				"reason":                   reason,
				"message":                  message + "。本工具只检查本地状态，不验证上游账户鉴权、配额或网络连通性。",
				"max_whitelist_entries":    5,
				"quota_hint":               xiequQuotaMessage,
				"state":                    s.m.xiequStateView(),
			}
			return jsonResult(out)
		},
	})
}

func (s *Server) xiequRefreshTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        "xiequ_whitelist_refresh",
		Description: "读取当前公网 IPv4，并确保它存在于谐趣代理池白名单中。只使用服务端保存的 UID/UKEY，不接受令牌参数；先读取白名单，缺失时添加并再次读取验证。配额满时只会删除本工具先前添加且已不在当前出口的 IP，绝不删除手工记录或 all。账户最多 5 个白名单；按次/流量计费产品可按客服规则申请增加，包时/包日/包月产品不能增加。",
		Schema:      xiequSchema(),
		ReadOnly:    func(json.RawMessage) bool { return false },
		Concurrent:  func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if ctx.Err() != nil {
				return actool.Errorf("谐趣白名单刷新已取消"), nil
			}
			agentKey, trusted := ctx.Value(xiequAgentContextKey{}).(string)
			if !trusted || agentKey == "" {
				return actool.Errorf("谐趣白名单刷新缺少可信 Agent 身份"), nil
			}
			if decodeProxyToolInput(input, xiequSchema(), nil) != nil {
				return actool.Errorf("谐趣白名单刷新工具不接受参数，请使用空对象调用"), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("谐趣白名单配置存储不可用，无法刷新"), nil
			}
			row, err := s.m.pg.GetTool("xiequ_whitelist_refresh")
			if err != nil || row == nil || !row.System || !row.Enabled || !contains(row.Agents, agentKey) {
				return actool.Errorf("当前 Agent 未获谐趣白名单刷新工具权限或工具已禁用"), nil
			}
			result, err := s.m.refreshXiequWhitelist(ctx, true)
			if err != nil {
				return actool.Errorf("谐趣白名单刷新失败，请检查配置和网络"), nil
			}
			if !result.OK && result.Reason == "quota_full" && !strings.Contains(result.Message, xiequQuotaMessage) {
				result.Message = result.Message + " " + xiequQuotaMessage
			}
			return jsonResult(result)
		},
	})
}

func (s *Server) xiequTools() []actool.CoreTool {
	tools := []actool.CoreTool{s.xiequStatusTool()}
	if s.m == nil || s.m.pg == nil {
		return tools
	}
	config, err := s.m.pg.XiequConfig()
	if err != nil || !config.Enabled || config.UID == "" || config.UKEY == "" {
		return tools
	}
	return append(tools, s.xiequRefreshTool())
}

func (s *Server) seedXiequTools() error {
	agents, _ := json.Marshal([]string{"worker", "auto", "pentest"})
	for _, tool := range []actool.CoreTool{s.xiequRefreshTool(), s.xiequStatusTool()} {
		schema, err := json.Marshal(tool.InputSchema())
		if err != nil {
			return err
		}
		if err := s.m.pg.SeedTool(tool.Name(), tool.Description(), schema, agents); err != nil {
			return err
		}
	}
	return s.seedXiequDefaultBindings()
}

func (s *Server) seedXiequDefaultBindings() error {
	const flag = "xiequ_tools_default_bindings_v1"
	if value, _, _ := s.m.pg.GetSetting(flag); value == "true" {
		return nil
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{"xiequ_whitelist_status", "xiequ_whitelist_refresh"}); err != nil {
		return err
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"xiequ_whitelist_status", "xiequ_whitelist_refresh"}); err != nil {
		return err
	}
	if err := s.m.pg.AddAgentToToolBinding("pentest", []string{"xiequ_whitelist_status", "xiequ_whitelist_refresh"}); err != nil {
		return err
	}
	return s.m.pg.SetSetting(flag, "true")
}

func isXiequTool(name string) bool {
	return name == "xiequ_whitelist_status" || name == "xiequ_whitelist_refresh"
}

type authorizedXiequTool struct {
	actool.CoreTool
	pg       *db.DB
	agentKey string
}

func (t authorizedXiequTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if ctx.Err() != nil {
		return actool.Errorf("谐趣白名单工具调用已取消"), nil
	}
	row, err := t.pg.GetTool(t.Name())
	if err != nil || row == nil || !row.System || !row.Enabled || !contains(row.Agents, t.agentKey) {
		return actool.Errorf("当前 Agent 未获谐趣白名单工具权限或工具已禁用"), nil
	}
	if len(in) > 16384 || decodeProxyToolInput(in, xiequSchema(), nil) != nil {
		return actool.Errorf("谐趣白名单工具不接受参数，请使用空对象调用"), nil
	}
	return t.CoreTool.Call(context.WithValue(ctx, xiequAgentContextKey{}, t.agentKey), in, tc)
}
