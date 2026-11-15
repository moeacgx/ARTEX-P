package server

import (
	"net/http"
	"regexp"
	"strings"
)

type mcpAccessTool struct {
	Name                     string
	Description              string
	Scope                    string
	Method                   string
	Path                     string
	Handler                  http.HandlerFunc
	Upload                   bool
	Download                 bool
	InjectRequestID          bool
	ResultErrorField         string
	ResultSuccessField       string
	EnsureFindingsPagination bool
}

var mcpPathParam = regexp.MustCompile(`\{([a-z]+)(\.\.\.)?\}`)

func (t mcpAccessTool) schema() map[string]any {
	props := map[string]any{"query": map[string]any{"type": "object", "description": "可选查询参数；值为字符串，例如 task、task_id、page、limit、size、path、overwrite", "additionalProperties": map[string]any{"type": "string"}}}
	required := []string{}
	for _, m := range mcpPathParam.FindAllStringSubmatch(t.Path, -1) {
		props[m[1]] = map[string]any{"type": "string", "minLength": 1, "description": "资源路径参数 " + m[1]}
		required = append(required, m[1])
	}
	if t.Method != "GET" {
		props["request_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$", "description": "本次操作唯一标识；重试必须使用相同标识和原参数"}
		required = append(required, "request_id")
		if !t.Upload {
			props["body"] = map[string]any{"type": "object", "description": t.Description, "additionalProperties": true}
			if t.InjectRequestID {
				required = append(required, "body")
			}
		}
	}
	if t.Upload {
		props["filename"] = map[string]any{"type": "string", "description": "文件名，不含目录"}
		props["content_base64"] = map[string]any{"type": "string", "description": "文件标准 base64 编码，解码后最多 8 MiB"}
		required = append(required, "filename", "content_base64")
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func (s *Server) mcpAccessTools() []mcpAccessTool {
	var tools []mcpAccessTool
	add := func(name, desc, scope, method, path string, h http.HandlerFunc) {
		tools = append(tools, mcpAccessTool{Name: "artex_" + name, Description: desc, Scope: scope, Method: method, Path: path, Handler: h})
	}
	add("tasks_list", "列出任务；此接口忽略查询参数并返回默认列表。", "tasks.read", "GET", "/tasks", s.listTasks)
	add("tasks_get", "读取任务及当前状态。", "tasks.read", "GET", "/tasks/{id}", s.getTask)
	add("tasks_create", "创建并按现有流程启动任务。body: description、goal；其他任务创建配置与网页一致。", "tasks.create", "POST", "/tasks", s.createTask)
	add("tasks_control", "控制任务。body: action（pause/resume）。", "tasks.control", "POST", "/tasks/{id}/control", s.control)
	add("tasks_message", "向 Worker 意图发送消息。body: message；调用级 request_id 会自动写入业务请求体。", "tasks.message", "POST", "/tasks/{id}/intents/{iid}/messages", s.sendWorkerMessage)
	tools[len(tools)-1].InjectRequestID = true
	add("tasks_chat", "向任务主 Agent 发送消息。body: message，可含 attachments。会消耗模型额度。", "tasks.message", "POST", "/tasks/{id}/chat", s.localAgentTaskChat)
	add("tasks_chat_session", "新建任务主 Agent 会话。", "tasks.message", "POST", "/tasks/{id}/chat/session", s.localAgentNewTaskChatSession)
	add("tasks_chat_upload", "向任务主 Agent 对话上传文件。", "tasks.message", "POST", "/chat/upload", s.chatUpload)
	add("tasks_chat_status", "查询任务主 Agent 执行状态。", "tasks.read", "GET", "/tasks/{id}/chat/status", s.taskChatStatus)
	add("tasks_chat_stop", "停止任务主 Agent 当前对话。", "tasks.message", "POST", "/tasks/{id}/chat/stop", s.stopChat)
	add("tasks_llm_get", "读取任务模型解析结果。", "llm.read", "GET", "/tasks/{id}/llm/resolution", s.taskLLMResolutionHandler)
	add("tasks_llm_set", "更新任务模型绑定，body 与任务模型配置接口一致。", "llm.write", "PUT", "/tasks/{id}/llm", s.updateTaskLLMProfiles)
	add("tasks_coverage", "读取任务覆盖统计。", "discovery.read", "GET", "/tasks/{id}/coverage", s.taskCoverage)
	add("tasks_coverage_graph", "读取任务覆盖图。", "discovery.read", "GET", "/tasks/{id}/coverage-graph", s.taskCoverageGraph)
	add("tasks_asset_refs", "读取任务资产引用。", "discovery.read", "GET", "/tasks/{id}/asset-refs", s.taskAssetRefs)
	add("skills_list", "列出已安装 Skills。", "skills.read", "GET", "/skills", s.fsListSkills)
	add("skills_upload", "上传 Skill ZIP；query.overwrite=true 才覆盖同名包。", "skills.upload", "POST", "/skills/upload", s.fsUploadSkill)
	add("skills_download", "下载 Skill ZIP，返回 base64。", "skills.read", "GET", "/skills/{name}/download", s.fsDownloadSkill)
	add("skills_delete", "删除指定 Skill。", "skills.write", "DELETE", "/skills/{name}", s.fsDeleteSkill)
	add("skills_files", "列出 Skill 内文件。", "skills.read", "GET", "/skills/{name}/files", s.fsListFiles)
	add("skills_read", "读取 Skill 文件；file 为 Skill 内相对路径。", "skills.read", "GET", "/skills/{name}/files/{file...}", s.fsReadFile)
	add("skills_write", "写入 Skill 文件。body: content。", "skills.write", "PUT", "/skills/{name}/files/{file...}", s.fsWriteFile)
	add("skills_remove_file", "删除 Skill 内指定文件。", "skills.write", "DELETE", "/skills/{name}/files/{file...}", s.fsDeletePath)
	add("assets_list", "列出资产。query: task_id、company_id、type、dsl、limit、offset。", "assets.read", "GET", "/assets", s.listAssets)
	add("assets_counts", "读取资产分类计数。", "assets.read", "GET", "/assets/counts", s.assetCounts)
	add("assets_insert", "添加资产；body 使用资产接口格式。", "assets.write", "POST", "/assets", s.insertAssets)
	add("assets_delete", "删除资产；body 使用资产接口格式。", "assets.delete", "DELETE", "/assets", s.deleteAssets)
	add("workspace_list", "列出工作区目录。query.path 为相对路径。", "workspace.read", "GET", "/workspace/list", s.wsList)
	add("workspace_read", "读取工作区文本文件。query.path 为相对路径。", "workspace.read", "GET", "/workspace/read", s.wsRead)
	add("workspace_write", "写入工作区文本。body: path、content。", "workspace.write", "POST", "/workspace/write", s.wsWrite)
	add("workspace_mkdir", "创建工作区目录。body: path。", "workspace.write", "POST", "/workspace/mkdir", s.wsMkdir)
	add("workspace_delete", "删除工作区文件。query.path 为相对路径。", "workspace.delete", "DELETE", "/workspace/delete", s.wsDelete)
	add("workspace_download", "下载工作区文件，返回 base64。query.path 为相对路径。", "workspace.download", "GET", "/workspace/download", s.wsDownload)
	add("workspace_upload", "上传文件到工作区。query.path 为目标目录。", "workspace.upload", "POST", "/workspace/upload", s.wsUpload)
	add("conversations_list", "列出对话。", "conversations.read", "GET", "/conversations", s.pgListConversations)
	add("conversations_agents", "列出可创建对话的 Agent 目录。", "conversations.read", "GET", "/conversations/agents", s.localAgentConversationAgents)
	add("conversations_create", "创建对话。body: agent_key、title，可指定模型配置。", "conversations.create", "POST", "/conversations", s.pgCreateConversation)
	add("conversations_messages", "读取对话消息。query: limit、before。", "conversations.read", "GET", "/conversations/{id}/messages", s.pgConversationMessages)
	add("conversations_message_detail", "读取对话中单条消息详情。", "conversations.read", "GET", "/conversations/{id}/messages/{seq}", s.pgConversationMsgDetail)
	add("conversations_send", "发送对话消息。body: message。会消耗模型额度。", "conversations.message", "POST", "/conversations/{id}/messages", s.pgSendConversationMessage)
	add("conversations_stop", "停止指定对话的当前执行。", "conversations.stop", "POST", "/conversations/{id}/stop", s.pgStopConversation)
	add("conversations_rename", "重命名对话。body: title。", "conversations.message", "PATCH", "/conversations/{id}", s.pgRenameConversation)
	add("conversations_profile", "更新对话模型配置。body 与对话配置接口一致。", "llm.write", "PATCH", "/conversations/{id}/profile", s.pgUpdateConversation)
	add("findings_list", "查询发现数据。query: task（切换为指定任务节点结果）或 task_id（全局发现按任务筛选）、severity、page、limit；全局筛选可省略分页参数。", "discovery.read", "GET", "/discovery/findings", s.findings)
	tools[len(tools)-1].EnsureFindingsPagination = true
	add("findings_get", "读取指定发现详情。", "discovery.read", "GET", "/discovery/findings/{id}", s.getFinding)
	add("findings_groups", "读取发现分组。query.task_id 可限定任务。", "discovery.read", "GET", "/discovery/findings/groups", s.findingGroups)
	add("findings_asset_tree", "读取发现资产树。query.task_id 可限定任务。", "discovery.read", "GET", "/discovery/findings/asset-tree", s.findingAssetTree)
	add("findings_stats", "读取发现统计。query.task 可限定任务。", "discovery.read", "GET", "/discovery/stats", s.localAgentFindingStats)
	add("findings_lineage", "读取发现来源链。", "discovery.read", "GET", "/discovery/findings/{id}/lineage", s.findingLineage)
	add("findings_retests", "读取发现复测记录。", "discovery.read", "GET", "/discovery/findings/{id}/retests", s.listFindingRetests)
	add("findings_active_retests", "读取进行中的复测。", "discovery.read", "GET", "/discovery/findings/retests/active", s.listActiveFindingRetests)
	add("findings_export", "导出发现数据，返回 base64 文件内容。", "discovery.read", "GET", "/discovery/findings/export", s.findingsExport)
	add("discovery_frontier", "读取探索前沿。query.task 可限定任务。", "discovery.read", "GET", "/discovery/frontier", s.frontier)
	add("discovery_graph", "读取探索图。query.task 可限定任务。", "discovery.read", "GET", "/discovery/graph", s.explorationGraph)
	add("discovery_nodes", "读取探索节点。query: task、page、size、kind、state、q、order。", "discovery.read", "GET", "/discovery/nodes", s.explorationNodes)
	add("discovery_activity", "读取当前活动。query.task 可限定任务。", "discovery.read", "GET", "/discovery/activity", s.activity)
	add("discovery_history", "读取历史活动。query.task 可限定任务。", "discovery.read", "GET", "/discovery/activity/history", s.activityHistory)
	add("discovery_activity_detail", "读取活动详情。query.task 可限定任务。", "discovery.read", "GET", "/discovery/activity/{seq}", s.activityDetail)
	add("discovery_intents", "读取任务意图。query.task 可限定任务。", "discovery.read", "GET", "/discovery/intents", s.intents)
	add("discovery_sessions", "读取主 Agent 会话。query.task 可限定任务。", "discovery.read", "GET", "/discovery/main-sessions", s.mainSessions)
	add("discovery_tokens", "读取 Token 用量。query.task 可限定任务。", "discovery.read", "GET", "/discovery/tokens", s.tokenStats)
	add("logs_list", "读取运行日志。query: limit、level。", "logs.read", "GET", "/logs", s.getLogs)
	add("logs_history", "读取历史运行日志。", "logs.read", "GET", "/logs/history", s.getLogsHistory)
	add("llm_get", "读取全局模型配置，不返回密钥；代理 URL 凭据会脱敏。", "llm.read", "GET", "/llm", s.mcpAccessGetLLM)
	add("llm_set", "保存全局模型配置。body 与 LLM 配置接口一致。", "llm.write", "POST", "/llm", s.setLLM)
	add("llm_test", "测试已保存的模型配置。必须提供 body.profile_id；不会接受自定义地址、密钥或代理参数。可能消耗额度。", "llm.test", "POST", "/llm/test", s.mcpAccessTestLLM)
	tools[len(tools)-1].ResultSuccessField = "ok"
	add("llm_profiles", "列出模型渠道，不返回密钥。", "llm.read", "GET", "/llm/profiles", s.pgListProfiles)
	add("llm_profiles_save", "新增或更新渠道。body: id（更新时）、name 及渠道配置。", "llm.write", "POST", "/llm/profiles", s.pgSaveProfile)
	add("llm_profiles_delete", "删除模型渠道。", "llm.write", "DELETE", "/llm/profiles/{id}", s.pgDeleteProfile)
	add("llm_profiles_activate", "激活模型渠道。body: id。", "llm.write", "POST", "/llm/profiles/active", s.pgActivateProfile)
	add("llm_retry_get", "读取模型重试策略。", "llm.read", "GET", "/llm/retry-policy", s.pgGetLLMRetryPolicy)
	add("llm_retry_set", "保存模型重试策略。body 为策略配置。", "llm.write", "POST", "/llm/retry-policy", s.pgSaveLLMRetryPolicy)
	add("llm_pool", "读取模型池状态。", "llm.read", "GET", "/llm/pool", s.pgLLMPoolStatus)
	add("llm_pool_reset", "重置模型池熔断状态。body 为重置目标。", "llm.write", "POST", "/llm/pool/reset", s.pgLLMPoolReset)
	add("llm_models", "读取渠道模型。", "llm.read", "GET", "/llm/profiles/{id}/models", s.pgGetLLMModels)
	add("llm_models_set", "保存渠道模型。body 为模型配置。", "llm.write", "PUT", "/llm/profiles/{id}/models", s.pgPutLLMModels)
	add("llm_routes", "读取模型路由。", "llm.read", "GET", "/llm/pool/routes", s.pgGetLLMPoolModelRoutes)
	add("llm_routes_set", "保存模型路由。body 为路由配置。", "llm.write", "PUT", "/llm/pool/routes/{id}", s.pgPutLLMPoolModelRoute)
	add("llm_records", "列出模型调用记录。query: task、page、size、model、session。", "llm.read", "GET", "/llm/records", s.pgListLLMRecords)
	add("llm_records_get", "读取模型调用详情，可能含业务对话内容。", "llm.read", "GET", "/llm/records/{id}", s.pgGetLLMRecord)
	add("llm_record_tasks", "列出有模型调用记录的任务。", "llm.read", "GET", "/llm/records/tasks", s.pgLLMTasks)
	add("llm_usage", "按模型汇总 Token。", "llm.read", "GET", "/llm/records/by-model", s.pgTokenByModel)
	add("tools_list", "列出工具目录及配置。", "tools.read", "GET", "/tools", s.pgListTools)
	add("tools_update", "更新工具配置。body 与工具配置接口一致。", "tools.write", "PUT", "/tools/{key}", s.pgUpdateTool)
	add("tools_create", "创建自定义工具。body 包含工具定义。", "tools.write", "POST", "/tools/custom", s.pgCreateCustomTool)
	add("tools_test", "执行自定义工具测试。body 为测试定义及参数，可能运行命令。", "tools.write", "POST", "/tools/custom/test", s.pgTestCustomTool)
	tools[len(tools)-1].ResultErrorField = "is_error"
	add("tools_custom_update", "更新自定义工具定义。", "tools.write", "PUT", "/tools/custom/{key}", s.pgUpdateCustomTool)
	add("tools_delete", "删除自定义工具。", "tools.write", "DELETE", "/tools/custom/{key}", s.pgDeleteCustomTool)
	add("tools_reset", "恢复工具默认配置。", "tools.write", "POST", "/tools/{key}/reset", s.pgResetTool)
	add("mcp_servers", "列出外部 MCP 服务，隐藏凭据及连接参数。", "mcp.read", "GET", "/mcp", s.localAgentMCPServers)
	add("mcp_save", "新增或更新外部 MCP。body: id（更新时）、name、transport、command/args 或 url、env、enabled。", "mcp.write", "POST", "/mcp", s.pgSaveMCP)
	add("mcp_delete", "删除外部 MCP 配置。", "mcp.write", "DELETE", "/mcp/{id}", s.pgDeleteMCP)
	add("mcp_tools", "读取外部 MCP 工具目录。", "mcp.read", "GET", "/mcp/{id}/tools", s.pgMCPTools)
	add("mcp_refresh", "连接外部 MCP 并刷新工具目录。", "mcp.write", "POST", "/mcp/{id}/refresh", s.pgRefreshMCP)
	add("proxies_list", "列出代理及脱敏状态。", "proxies.read", "GET", "/proxies", s.listProxies)
	add("proxies_get", "读取代理详情。", "proxies.read", "GET", "/proxies/{id}", s.getProxy)
	add("proxies_test", "检测指定代理，遵守代理使用约束。", "proxies.test", "POST", "/proxies/{id}/test", s.testLocalAgentProxy)
	for i := range tools {
		tools[i].Upload = strings.HasSuffix(tools[i].Name, "_upload")
		tools[i].Download = strings.HasSuffix(tools[i].Name, "_download")
		if tools[i].Name == "artex_findings_export" {
			tools[i].Download = true
		}
	}
	return tools
}
