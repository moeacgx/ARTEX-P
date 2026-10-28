---
name: artex-agent
description: 通过本机 Ed25519 设备身份直接操作 ARTEX：查询和检测代理、管理任务、工作区、资产、对话、发现数据、日志、LLM 和工具；无需管理员密码或平台 Token。
---

# ARTEX 本地 Agent

使用本 Skill 的 `scripts/artex-agent.mjs`，要求 Node.js 20 或更高版本，无需安装 npm 包。

已配对设备直接执行目标业务命令，不需要每次先查 `auth status`。只在首次接入、身份未配置或收到 401 时检查认证；403 若已明确给出缺少的 scope，直接在网页补充权限，不必额外查认证。

常见请求直接使用：

```text
node <脚本> proxies list
node <脚本> proxies get --id <代理ID>
node <脚本> proxies test --id <代理ID>
node <脚本> tasks list
node <脚本> workspace list
node <脚本> assets counts
node <脚本> llm status
node <脚本> tools list
node <脚本> logs list --limit 20
```

“查代理 / 给我代理 IP”先运行 `proxies list`，从结果读取代理 ID、地址及最近检测结果；用户要求验证是否可用或读取普通代理的实时出口 IP 时，对指定 ID 运行 `proxies test`。检测结果中 `status=healthy` 表示本次检测成功，`egress_ip` 是出口 IP，`last_checked_at` 是检测时间；列表仅代表最近状态，普通代理的出口 IP 仅在本次检测响应中返回。失败可能返回 HTTP 502，检测冲突返回 HTTP 409。不要为这些查询创建任务、对话或调用 LLM，也不通过 CloudSSH 绕行。列表无检测结果时明确说明尚未检测；出口 IP 为空时明确说明未取得，不把配置地址当作出口 IP。

每个业务命令只请求一次目标接口，不自动登录、探测其他接口或重试。普通查询默认 15 秒、代理检测 45 秒，写入与下载 60 秒；可用 `--timeout-ms 30000` 调整，允许 1000–300000 毫秒。超时和网络断开会分别提示；写请求结果不确定时先核对目标状态，不能盲目重放。403 若已显示所缺权限，在网页补充批准该 scope 后直接执行原业务；若未说明所缺权限，再用 `auth status` 检查审批状态。不能靠反复配对解决。

首次接入调用：

```text
node <本Skill目录>/scripts/artex-agent.mjs auth login --url https://artex.example.com --name 本地Agent
```

该命令在本机生成设备私钥并提交公钥配对申请，显示设备 ID 和 SHA-256 公钥指纹。
让用户在 ARTEX 网页“系统 / Agent 接入”核对指纹、设备名称和申请时间，再审批所需权限。
审批后再次调用 `auth status`，确认 `status` 为 `approved` 并检查 `scopes`。
未批准或已撤销时停止业务操作，不借用管理员 JWT 绕过设备权限。

```text
node <脚本> tasks list
node <脚本> tasks get --id <任务ID>
node <脚本> tasks create --description "任务描述" --goal "执行目标及明确授权范围"
node <脚本> tasks create --file <本地任务JSON文件>
node <脚本> tasks control --id <任务ID> --action pause|resume
node <脚本> tasks chat --id <任务ID> --message "发给主 Agent 的消息"
node <脚本> tasks chat-session --id <任务ID>
node <脚本> tasks chat-status --id <任务ID>
node <脚本> tasks chat-stop --id <任务ID>
node <脚本> tasks worker-message --id <任务ID> --intent-id <意图ID> --message "发给 Worker 的消息"
node <脚本> tasks llm-status --id <任务ID>
node <脚本> tasks llm-set --id <任务ID> --file <LLM配置JSON>
node <脚本> skills list
node <脚本> skills upload --file <本地Skill.zip>
node <脚本> workspace list [--path <目录>]
node <脚本> workspace read --path <文件>
node <脚本> workspace write --path <文件> (--content <内容> | --file <本地文本文件>)
node <脚本> workspace mkdir --path <目录>
node <脚本> workspace delete --path <文件或目录>
node <脚本> workspace upload --file <本地文件> [--path <工作区目录>]
node <脚本> workspace download --path <工作区文件> --output <本地目标文件>
node <脚本> assets list [--type <类型>] [--company-id <ID>] [--task-id <ID>]
node <脚本> assets counts
node <脚本> assets upload --file <资产JSON文件>
node <脚本> assets download --output <本地JSON文件> [--type <类型>] [--company-id <ID>] [--task-id <ID>]
node <脚本> assets delete --ids <ID列表，例如 1,2,3>
node <脚本> conversations list
node <脚本> conversations create --agent-key <Agent键> [--title <标题>]
node <脚本> conversations messages --id <会话ID> [--since <序号>] [--before <序号>] [--limit <数量>]
node <脚本> conversations send --id <会话ID> --message <消息>
node <脚本> conversations stop --id <会话ID>
node <脚本> discover findings|stats|graph|nodes|activity [--task-id <ID>]
node <脚本> logs list [--since <序号>] [--limit <数量>]
node <脚本> logs history [--before <ID>] [--limit <数量>]
node <脚本> llm status
node <脚本> llm profiles
node <脚本> llm set --file <全局LLM配置JSON>
node <脚本> llm profile-save --file <LLM档案JSON>
node <脚本> llm activate --id <档案ID>
node <脚本> tools list
node <脚本> tools set --key <工具键> --file <工具JSON>
node <脚本> tools custom-save --file <自定义工具JSON>
node <脚本> tools custom-delete --key <工具键>
```

- `tasks.read` 允许查询任务，`tasks.create` 允许创建任务。
- `proxies.read` 允许查询代理，`proxies.test` 允许检测指定代理；检测使用 ARTEX 已配置的探测目标，不需要用户提供任意 URL。代理返回信息不包含认证密码或配置秘密。
- `tasks.control` 允许暂停或恢复任务；`tasks.message` 允许向任务主 Agent 发送消息、创建主会话、停止主 Agent 轮次，或向暂停的 Worker 意图发送消息。
- `tasks llm-status` 查看任务当前模型解析结果；`tasks llm-set` 按 ARTEX 接口格式更新该任务的 LLM 配置，需要 `llm.write` 权限。
- `skills.read` 允许查询 Skill，`skills.upload` 允许上传 Skill ZIP。
- `workspace.read`、`workspace.download` 允许浏览/读取/下载工作区；`workspace.write` 和 `workspace.upload` 可写入文件。工作区路径由 ARTEX 服务端限制在任务工作目录内。
- `workspace.write` 还允许创建目录，`workspace.delete` 允许删除工作区内的文件或目录树；删除操作不可恢复，需明确确认目标路径。
- `assets.read`、`discovery.read`、`logs.read`、`llm.read`、`tools.read` 分别允许读取资产、发现页数据、运行日志、LLM 状态/配置档案和工具目录。发现查询的 `--task-id` 会绑定任务过滤；`findings` 支持 `--page`、`--limit`、`--filter`，`nodes` 支持 `--page`、`--size`、`--kind`、`--state`、`--q`、`--order`。
- `assets upload` 使用 ARTEX 资产接口的 JSON 文件格式（对象需包含 `assets` 数组，可选 `task_id`）；`assets download` 将筛选后的 JSON 响应保存到本地且拒绝覆盖已有文件；`assets delete` 需要明确的正整数 ID 列表和 `assets.delete` 权限。
- `conversations.read` 允许查询对话与消息；`conversations.create` 创建对话；`conversations.message` 发送消息并启动主 Agent 执行；`conversations.stop` 停止该会话正在运行的轮次。
- `llm set`、`llm profile-save`、`llm activate` 需要 `llm.write`，会更改全局运行中的 LLM 配置或激活档案；发送配置文件前检查目标地址和模型档案，勿在终端输出 API Key。
- `tools set`、`tools custom-save`、`tools custom-delete` 需要 `tools.write`，会改变 Agent 可用工具或其执行配置；删除前确认工具键，服务端会按现有校验处理工具定义。
- 创建任务复用 ARTEX 的现有创建和启动流程，会开始规划与执行；仅在用户要求执行该任务且目标授权范围明确时创建。
- `conversations send` 会触发 Agent 处理。发送前先确认目标会话和用户要求；网络中断后先查询消息和会话状态再决定是否重试。
- `workspace write` 会创建或覆盖目标文件；`workspace upload` 上传同名文件可能覆盖已有内容。下载默认拒绝覆盖本地同名目标。
- 任务 JSON 使用 ARTEX 现有创建接口字段，例如 `description`、`goal`、`name`、`source_task_ids`、`proxy_mode`、`proxy_id`，不编造资源 ID。代理和模型配置由 ARTEX 原有校验处理。
- Skill ZIP 内应包含 `SKILL.md` 和所需文件，压缩包须小于 20 MiB；默认遇到同名 Skill 返回冲突。仅在用户明确要求覆盖时添加 `--overwrite`。
- 写请求网络中断后，脚本不会自动重放。先通过网页或列表确认任务、Skill 是否已创建，再决定是否重试。

Windows 私钥使用当前用户 DPAPI 加密保存；其他系统保存于当前用户私有目录，权限为 0600。
不读取或打印身份文件、私钥或管理员凭据。仅记录公开设备 ID、指纹、授权状态和必要业务结果。
HTTP 仅允许本机回环地址，其他部署使用 HTTPS。需要多个独立身份时设置 `ARTEX_AGENT_CONFIG_DIR`。
撤销访问在 ARTEX 网页完成；本机删除身份文件不等于服务端撤销设备。

## 技能更新通知

每次请求会根据本地 Skill 实际文件计算内容版本，并随设备签名上报。
ARTEX 网页“本地 Agent 接入”显示各设备是否需要更新；旧版单文件或文件残缺时显示版本未知。
下载新版 `artex-agent.zip` 后，将其中 `artex-agent` 目录完整覆盖当前 Skill 目录，包含 `skill-manifest.json`。
更新后执行 `node <脚本> auth status`，让网页确认新版本；仅下载压缩包不代表设备已升级。
更新只替换 Skill 文件，保留原设备身份目录，不重新配对或重新申请审批。
