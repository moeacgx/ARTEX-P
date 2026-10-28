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

常规业务命令只请求一次目标接口；`assets download --all` 每页请求一次，两者均不自动登录、探测其他接口或重试。普通查询默认 15 秒、代理检测 45 秒，写入与下载 60 秒；可用 `--timeout-ms 30000` 调整，允许 1000–300000 毫秒，分页导出按每次请求计时。超时和网络断开会分别提示；写请求结果不确定时先核对目标状态，不能盲目重放。403 若已显示所缺权限，在网页补充批准该 scope 后直接执行原业务；若未说明所缺权限，再用 `auth status` 检查审批状态。不能靠反复配对解决。

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
node <脚本> tasks coverage --id <任务ID>
node <脚本> tasks coverage-graph --id <任务ID>
node <脚本> tasks asset-refs --id <任务ID> --asset-id <资产ID>
node <脚本> skills list
node <脚本> skills upload --file <本地Skill.zip> [--overwrite]
node <脚本> skills download --name <Skill名称> --output <本地ZIP文件>
node <脚本> skills files --name <Skill名称>
node <脚本> skills read --name <Skill名称> --path <包内文件>
node <脚本> skills write --name <Skill名称> --path <包内文件> (--content <内容> | --file <本地文件>)
node <脚本> skills delete --name <Skill名称>
node <脚本> skills delete-file --name <Skill名称> --path <包内文件>
node <脚本> workspace list [--path <目录>]
node <脚本> workspace read --path <文件>
node <脚本> workspace write --path <文件> (--content <内容> | --file <本地文本文件>)
node <脚本> workspace mkdir --path <目录>
node <脚本> workspace delete --path <文件或目录>
node <脚本> workspace upload --file <本地文件> [--path <工作区目录>]
node <脚本> workspace download --path <工作区文件> --output <本地目标文件>
node <脚本> assets list [--type <类型>] [--company-id <ID>] [--task-id <ID>]
node <脚本> assets counts [--task-id <ID>]
node <脚本> assets upload --file <资产JSON文件>
node <脚本> assets download --output <本地JSON文件> [--type <类型>] [--company-id <ID>] [--task-id <ID>] [--dsl <筛选>] [--all]
node <脚本> assets delete --ids <ID列表，例如 1,2,3>
node <脚本> conversations agents
node <脚本> conversations list
node <脚本> conversations create --agent-key <Agent键> [--title <标题>]
node <脚本> conversations messages --id <会话ID> [--since <序号>] [--before <序号>] [--limit <数量>]
node <脚本> conversations send --id <会话ID> --message <消息>
node <脚本> conversations stop --id <会话ID>
node <脚本> discover findings|stats|graph|nodes|activity [--task-id <ID>]
node <脚本> discover frontier|intents|main-sessions|tokens [--task-id <ID>]
node <脚本> discover finding --id <发现ID> [--context-task <任务ID>]
node <脚本> discover lineage|retests --id <发现ID>
node <脚本> discover activity-detail --id <活动序号> [--task-id <ID>]
node <脚本> logs list [--since <序号>] [--limit <数量>]
node <脚本> logs history [--before <ID>] [--limit <数量>]
node <脚本> llm status
node <脚本> llm profiles
node <脚本> llm pool
node <脚本> llm retry-policy
node <脚本> llm models --id <档案ID>
node <脚本> llm set --file <全局LLM配置JSON>
node <脚本> llm profile-save --file <LLM档案JSON>
node <脚本> llm activate --id <档案ID>
node <脚本> tools list
node <脚本> tools set --key <工具键> --file <工具JSON>
node <脚本> tools custom-save --file <自定义工具JSON>
node <脚本> tools custom-update --key <工具键> --file <自定义工具JSON>
node <脚本> tools custom-delete --key <工具键>
node <脚本> mcp list
node <脚本> mcp tools --id <MCP服务器ID>
```

- `tasks.read` 允许查询任务，`tasks.create` 允许创建任务。
- `proxies.read` 允许查询代理，`proxies.test` 允许检测指定代理；检测使用 ARTEX 已配置的探测目标，不需要用户提供任意 URL。代理返回信息不包含认证密码或配置秘密。
- `tasks.control` 允许暂停或恢复任务；`tasks.message` 允许向任务主 Agent 发送消息、创建主会话、停止主 Agent 轮次，或向暂停的 Worker 意图发送消息。
- `tasks llm-status` 查看任务当前模型解析结果；`tasks llm-set` 按 ARTEX 接口格式更新该任务的 LLM 配置，需要 `llm.write` 权限。
- `skills.read` 允许查询、读取和下载 Skill；`skills.upload` 允许上传 Skill ZIP；`skills.write` 允许修改包内文件或删除文件、目录及整个 Skill。包名与包内路径分开传入，路径使用相对路径与正斜杠，不含 `.`、`..` 或空段。
- `workspace.read`、`workspace.download` 允许浏览/读取/下载工作区；`workspace.write` 和 `workspace.upload` 可写入文件。工作区是 ARTEX 服务端所有 Agent 共用的工作目录，不按当前任务隔离；先查路径，避免覆盖其他任务产物。内联读取上限 2 MiB，返回 `too_large` 或 `binary` 时改用下载；上传请求上限 512 MiB（包含表单开销）。
- `workspace.write` 还允许创建目录，`workspace.delete` 允许删除工作区内的文件或目录树；删除操作不可恢复，需明确确认目标路径。
- `assets.read`、`discovery.read`、`logs.read`、`llm.read`、`tools.read` 分别允许读取资产、发现页数据、运行日志、LLM 状态/配置档案和工具目录；`mcp.read` 读取 MCP 服务器及工具目录。任务覆盖查询需要 `discovery.read`。列表按需要过滤，勿把大列表全文当作必需上下文。
- 查询指定任务的发现数据时始终传 `--task-id`，避免使用服务端默认任务。`stats`、`findings` 不传任务为全局统计/列表。`findings` 支持 `--page`、`--limit`、`--filter`（任务视图存在固定上限，不能视为全量）；`nodes` 支持 `--page`、`--size`、`--kind`、`--state`、`--q`、`--order`；`activity` 支持 `--since`、`--limit`；`stats` 和 `graph` 仅接受任务筛选。`frontier` 支持 `--limit`；`intents` 默认最多 300 条，用 `--page 1 --limit 100` 取得 `{items,has_more}`，再以末条意图 ID 传 `--before` 续读至 `has_more=false`（page 只是启用游标分页，不按页码跳转）。先查列表取得真实发现 ID、活动序号，再查详情。
- `assets upload` 需要 `assets.write`，JSON 对象包含 `assets` 数组，可选真实 `task_id`。上传响应可能包含逐条错误，需检查结果，不能只依据 HTTP 200 判断全部成功。`assets delete` 需要明确的正整数 ID 列表和 `assets.delete` 权限。
- `assets list` 和单页 `assets download` 在未提供类型、DSL、公司或任务范围时默认只查 `root_domain`；任务/公司范围或 DSL 查询不指定类型时可返回多种类型。默认每页 50 条、最多 200 条；支持 `--limit`、`--offset`、`--dsl`。DSL 优先于公司筛选，公司范围优先于任务范围，避免混用被忽略的筛选。
- 完整分页下载使用 `--all`，必须显式指定 `--type`、`--task-id`、`--company-id`、`--dsl` 中至少一项，不能混用 `--limit` 或 `--offset`。例如 `assets download --type root_domain --all --output domains.json`。检查命令输出的 `count`、`total`、`complete`，未完成不能称为全量。数量变化、重复 ID、空页或请求失败时不写最终文件；分页并非一致性快照，同数量内容变化仍可能无法识别。导出是查询结果，虽含 `assets` 数组，导入仍按写入接口字段处理，不恢复原 ID、公司关联、全部 `task_ids` 等元数据；应用描述等字段需按写入格式转换，不能当作无损备份。下载均拒绝覆盖本地已有文件。
- `conversations.read` 允许查询可用 Agent、对话与消息；创建前用 `conversations agents` 取得真实 Agent 键。`conversations.create` 创建对话；`conversations.message` 发送消息并启动主 Agent 执行；`conversations.stop` 停止该会话正在运行的轮次，不接受消息参数。
- `llm set`、`llm profile-save`、`llm activate` 需要 `llm.write`，会更改全局运行中的 LLM 配置或激活档案；发送配置文件前检查目标地址和模型档案，勿在终端输出 API Key。
- `tools set`、`tools custom-save`、`tools custom-update`、`tools custom-delete` 需要 `tools.write`。`set` 修改描述、Schema、Agent 绑定和启用状态；`custom-save` 新建，`custom-update` 修改已有自定义工具的执行定义，不会自动创建缺失工具。更新是完整字段替换，先查原定义保留所需字段；删除前确认工具键。
- 创建任务复用 ARTEX 的现有创建和启动流程，会开始规划与执行；仅在用户要求执行该任务且目标授权范围明确时创建。
- `conversations send` 会触发 Agent 处理。发送前先确认目标会话和用户要求；网络中断后先查询消息和会话状态再决定是否重试。
- `workspace write` 会创建或覆盖目标文件；`workspace upload` 上传同名文件可能覆盖已有内容。下载默认拒绝覆盖本地同名目标。
- 任务 JSON 使用 ARTEX 现有创建接口字段，例如 `description`、`goal`、`name`、`source_task_ids`、`proxy_mode`、`proxy_id`，不编造资源 ID。代理和模型配置由 ARTEX 原有校验处理。
- Skill ZIP 内应包含 `SKILL.md` 和所需文件，压缩包须小于 20 MiB；默认遇到同名 Skill 返回冲突。仅在用户明确要求覆盖时添加 `--overwrite`。
- 写请求网络中断后，脚本不会自动重放。先通过网页或列表确认任务、Skill 是否已创建，再决定是否重试。

## JSON 文件示例

以下展示接口字段，按用户授权替换目标后再发送；JSON 文件支持 UTF-8 BOM。仅检查配置时使用读取命令，不为验证格式创建任务、发送消息、执行工具或更换模型。

任务文件（`tasks create --file task.json` 会启动执行）：

```json
{"name":"文档核对","description":"只读核对指定工作区文档","goal":"仅阅读用户指定文件并返回摘要，不修改文件，不访问外网"}
```

资产文件（域名仅为结构示例，换成用户授权的真实目标；需关联任务时增加查询取得的 `task_id`）：

```json
{"assets":[{"type":"root_domain","domain":"example.com"}]}
```

### 切换任务当前模型，保留原链

用户说“切换到已有备用档案”时，只改变激活项，不删除原链成员、重排顺序或丢弃模型绑定。已明确授权的目标直接执行，不要求用户再次手工输入已查询到的 ID。

1. 用 `llm profiles` 匹配用户指定的真实档案；用 `tasks get --id <任务ID>` 读取完整 `members`、`llm_profile_ids` 和激活位置。`tasks llm-status` 只返回各角色的实际解析结果，不能用于取得完整原链。
2. 若 `members` 非空：完整保留每个成员的 `profile_id`、`model_mode`、`model_id` 及数组顺序，请求体只包含原 `members` 和目标成员的 `active_position`（从 0 开始）。不要改用 `llm_profile_ids`，否则会丢失成员的模型绑定。
3. 若没有成员定义而只有 `llm_profile_ids`：保留完整数组，仅设置 `active_llm_profile_id`。将所选结构写成 JSON，执行一次 `tasks llm-set --id <任务ID> --file task-llm.json`。不要混用两种结构，也不要把返回对象整体原样回传。
4. 回读 `tasks get` 核对原链、顺序和模型绑定未变，且激活项符合目标；再用 `tasks llm-status` 核对主 Agent 等角色的 `profile_id`、模型与 `available`。若 `source=agent_binding`，任务设置不一定改变该角色；明确说明实际生效范围，不擅自改角色绑定或全局档案。只有确认用户要求的目标角色已生效，才按原请求用 `tasks chat` 发送一次继续处理消息。
5. 写请求断线或超时：先按第 4 步回读，不盲重放；已生效则继续后续通知，仍无法确认则报告未确认。消息发送也只执行一次，接收成功或 `running=true` 不代表整个任务已完成。

**🔴 CHECKPOINT：** 档案名称重复、同档案有多个候选模型、目标不在当前链中或原配置无法完整读取时，先用对应读取命令定位；仍有歧义才询问用户是追加成员、选择哪个模型还是替换整链。仅在用户明确要求替换整条链时，才构造单档案链。不要因为当前解析结果只有一个档案，就推断原链也只有一个。

以下仅展示请求形状：只有实际查询所得 ID、成员和顺序恰为示例时才直接使用；其余情况从查询结果生成，不照抄示例 ID。旧式链原为 `[7,9]`、需激活 9：

```json
{"llm_profile_ids":[7,9],"active_llm_profile_id":9}
```

成员式链原有两个指定模型、需激活第二项时，保留原成员定义：

```json
{"members":[{"profile_id":"7","model_mode":"fixed-model","model_id":"71"},{"profile_id":"9","model_mode":"fixed-model","model_id":"91"}],"active_position":1}
```

LLM 档案（`llm profile-save --file profile.json`）：字段是 `format`，不是 `provider`。下面是结构模板，地址、模型和密钥必须在本地换成供应商的实际值；新建不填 `id`，编辑则先查档案并保留完整配置。不要打印密钥文件或把密钥写入对话、日志、版本库。

```json
{"name":"供应商档案","format":"openai","base_url":"https://api.example.com/v1","model":"供应商模型名","api_key":"仅在本地填写真实密钥","streaming":true,"proxy_mode":"inherit"}
```

自定义工具（`tools custom-save --file tool.json`）：`kind` 支持 `command`、`script`、`http`、`shell`；命令工具使用 `exec.command`。示例禁用且不绑定 Agent，启用前先查真实 Agent 键并按用户要求设置。

```json
{"key":"audit_echo","description":"返回固定审计文本","schema":{"type":"object","properties":{},"additionalProperties":false},"agents":[],"enabled":false,"kind":"command","exec":{"command":"printf audit_ok","timeout_ms":1000}}
```

编辑该自定义工具用 `tools custom-update --key audit_echo --file tool.json`。`tools set` 只接受 `description`、`schema`、`agents`、`enabled`，将原定义中的这四项完整保留并按需要修改，例如：

```json
{"description":"返回固定审计文本","schema":{"type":"object","properties":{},"additionalProperties":false},"agents":[],"enabled":false}
```

Windows 私钥使用当前用户 DPAPI 加密保存；其他系统保存于当前用户私有目录，权限为 0600。
不读取或打印身份文件、私钥或管理员凭据。仅记录公开设备 ID、指纹、授权状态和必要业务结果。
HTTP 仅允许本机回环地址，其他部署使用 HTTPS。需要多个独立身份时设置 `ARTEX_AGENT_CONFIG_DIR`。
撤销访问在 ARTEX 网页完成；本机删除身份文件不等于服务端撤销设备。

## 技能更新通知

每次请求会根据本地 Skill 实际文件计算内容版本，并随设备签名上报。
ARTEX 网页“本地 Agent 接入”显示各设备是否需要更新；旧版单文件或文件残缺时显示版本未知。
这里比较的是 Skill 包实际文件的内容版本，包含 `SKILL.md` 与脚本；不是网页接入提示词的独立版本或更新历史。设备已上报当前版本时，待更新提醒会消失；仅修改或复制网页提示词，不代表本地 Skill 已更新。
下载新版 `artex-agent.zip` 后，将其中 `artex-agent` 目录完整覆盖当前 Skill 目录，包含 `skill-manifest.json`。
更新后执行 `node <脚本> auth status`，让网页确认新版本；仅下载压缩包不代表设备已升级。
更新只替换 Skill 文件，保留原设备身份目录，不重新配对或重新申请审批。
