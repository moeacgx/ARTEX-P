package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/fofa"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func fofaSchema() map[string]any {
	return objSchema(map[string]any{
		"query":  strParam("FOFA 查询语句，例如 domain=\"example.com\"；必须符合当前任务范围，最多4096字节"),
		"page":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 1, "description": "页码；仅请求本页，不自动翻页"},
		"size":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "每页数量，默认20，最多100；实际消耗受FOFA账号规则限制"},
		"fields": map[string]any{"type": "array", "minItems": 0, "maxItems": 14, "items": map[string]any{"type": "string"}, "description": "可选字段：host,ip,port,protocol,domain,title,server,country,country_name,region,city,as_number,as_organization,icp；省略或空数组使用默认host,ip,port,protocol,title"},
	}, "query")
}

func (s *Server) fofaTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        "fofa_search",
		Description: "搜索 FOFA 已收录资产数据。适合域名/IP/服务线索查询，使用前可加载 fofa-search Skill。服务端管理令牌，参数中不要提交 API Key；请求使用当前任务/会话出口。每次只查一页，不自动导入资产；结果不是资产归属或漏洞存在的证明。搜索可能消耗账号额度。",
		Schema:      fofaSchema(),
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var q fofa.Query
			if len(in) > 16384 || decodeProxyToolInput(in, fofaSchema(), nil) != nil || json.Unmarshal(in, &q) != nil {
				return actool.Errorf("FOFA 查询参数无效；不要传入令牌或自定义接口地址"), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("FOFA 配置不可用"), nil
			}
			c, err := s.m.pg.FOFAConfig()
			if err != nil {
				return actool.Errorf("FOFA 配置不可用，请检查系统设置"), nil
			}
			if !c.Enabled || c.APIKey == "" {
				return actool.Errorf("FOFA 未启用或未配置，请在系统设置中保存 API Key 并启用"), nil
			}
			client, finish, err := s.admittedFOFAClient(ctx, c.APIKey)
			if err != nil {
				return actool.Errorf("FOFA 当前任务或出口不可用，未发起查询且未回退直连"), nil
			}
			defer finish()
			result, err := client.Search(ctx, q)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(result)
		},
	})
}

// 每轮装配读取配置，保存后无需重启。调用时再次核验，已装配的旧工具也不能越过禁用。
func (s *Server) fofaTools() []actool.CoreTool {
	tools := []actool.CoreTool{s.fofaStatusTool()}
	if s.m == nil || s.m.pg == nil {
		return tools
	}
	c, err := s.m.pg.FOFAConfig()
	if err != nil || !c.Enabled || c.APIKey == "" {
		return tools
	}
	return append(tools, s.fofaTool())
}

func (s *Server) seedFOFATool() error {
	for _, t := range []actool.CoreTool{s.fofaTool(), s.fofaStatusTool()} {
		schema, err := json.Marshal(t.InputSchema())
		if err != nil {
			return err
		}
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, json.RawMessage(`["worker","auto","pentest"]`)); err != nil {
			return err
		}
	}
	return nil
}

func isFOFATool(name string) bool { return name == "fofa_search" || name == "fofa_status" }

// 持有真实 IO 引用到读取和关闭响应结束。只跳过本地明文录制，绝不跳过既有出口策略。
func (s *Server) admittedFOFAClient(ctx context.Context, key string) (*fofa.Client, func(), error) {
	noop := func() {}
	subject, err := s.proxySubjectFromContext(ctx)
	if err != nil {
		return nil, noop, err
	}
	formal := ctx.Value(proxySubjectContextKey{}).(admittedProxySubject)
	policy, err := s.m.pg.GetProxyPolicy(subject)
	if err != nil || policy.State.Revision != formal.Revision {
		return nil, noop, db.ErrProxyRevisionConflict
	}
	if err = actool.CheckOperationAdmission(ctx, "fofa_search"); err != nil {
		return nil, noop, err
	}
	token, err := s.m.pg.AcquireProxyIOOperation(subject, policy.State.Revision, policy.State.ID)
	if err != nil {
		return nil, noop, err
	}
	release := func() {
		if e := s.m.pg.ReleaseProxyIOOperation(subject, token); e != nil {
			log.Printf("[fofa] 查询退出但出口引用释放失败，请按正式恢复流程处理")
		}
	}
	proxy, err := s.m.ResolveLLMProxy(ctx, proxySubjectRuntimeTask(policy), "")
	if err != nil {
		release()
		return nil, noop, err
	}
	tr, err := fofaTransport(proxy)
	if err != nil {
		release()
		return nil, noop, err
	}
	finish := func() { tr.CloseIdleConnections(); release() }
	client := fofa.New(&http.Client{Transport: proxyObservedTransport{base: tr, ctx: ctx, proxy: proxy}}, key)
	return client, finish, nil
}

type authorizedFOFATool struct {
	actool.CoreTool
	pg       *db.DB
	agentKey string
}

func (t authorizedFOFATool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if ctx.Err() != nil {
		return actool.Errorf("FOFA 查询已取消"), nil
	}
	row, err := t.pg.GetTool(t.Name())
	if err != nil || row == nil || !row.System || !row.Enabled || !contains(row.Agents, t.agentKey) {
		return actool.Errorf("当前 Agent 未获 FOFA 工具权限或工具已禁用"), nil
	}
	// 在默认值重编码和计量前验证原始输入，避免重复键或 null 被装饰器消掉。
	schema := fofaSchema()
	if t.Name() == "fofa_status" {
		schema = objSchema(map[string]any{})
	}
	if len(in) > 16384 || decodeProxyToolInput(in, schema, nil) != nil {
		return actool.Errorf("FOFA 查询参数无效；不要传入令牌或自定义接口地址"), nil
	}
	return t.CoreTool.Call(context.WithValue(ctx, fofaAgentContextKey{}, t.agentKey), in, tc)
}
