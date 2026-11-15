export interface MCPAccessKey {
  id: number;
  name: string;
  prefix: string;
  scopes: string[];
  created_at: string;
  expires_at: string;
  revoked_at: string | null;
  last_used_at: string | null;
}

export interface MCPAccessTool {
  name: string;
  description: string;
  scope: string;
  write: boolean;
  input_schema: Record<string, unknown>;
}

export interface MCPAccessOverview {
  keys: MCPAccessKey[];
  scopes: string[];
  tools: MCPAccessTool[];
  endpoint: string;
}

export interface MCPAccessCall {
  id: number;
  key_id: number;
  tool: string;
  request_id: string;
  status: string;
  status_code: number;
  duration_ms: number;
  created_at: string;
}

export interface MCPAccessCalls {
  calls: MCPAccessCall[];
  total: number;
  page: number;
  page_size: number;
}

export const mcpScopeGroups: Record<string, string> = {
  tasks: "任务",
  skills: "Skill",
  assets: "资产",
  workspace: "工作空间",
  conversations: "对话",
  discovery: "发现数据",
  logs: "日志",
  llm: "模型",
  tools: "工具",
  mcp: "外部 MCP",
  proxies: "代理",
};

export function mcpClientConfig(client: string, endpoint: string) {
  if (client === "codex") {
    return `[mcp_servers.artex]\nurl = ${JSON.stringify(endpoint)}\nbearer_token_env_var = "ARTEX_MCP_KEY"`;
  }
  const envPlaceholder = "${" + "env:ARTEX_MCP_KEY}";
  return JSON.stringify(
    { mcpServers: { artex: { url: endpoint, headers: { Authorization: `Bearer ${envPlaceholder}` } } } },
    null,
    2,
  );
}
