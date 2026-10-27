# 本地 Agent 接入

ARTEX 提供设备审批和 Ed25519 请求签名，使本机编码 Agent 可以查询任务、创建任务、查询 Skill 和上传 Skill，无需向 Agent 提供管理员 JWT。

## 快速使用

本机需要 Node.js 20 或更高版本，不需要安装 npm 依赖：

可在 ARTEX 网页“系统 / 本地 Agent 接入”下载完整 Skill ZIP，解压到本机 Agent 的 Skill 目录后按 `SKILL.md` 操作。该包内嵌于 ARTEX 程序中，服务器不需要保留源码。

同页会显示当前实例的 ARTEX 地址和首次接入提示词，分别复制给本机 Agent 即可。提示词会要求 Agent 展示设备 ID 与 SHA-256 指纹并等待网页审批；没有明确目标和授权范围时，不创建扫描任务。

```text
node scripts/artex-agent.mjs auth login --url https://artex.example.com --name 我的本地Agent
```

在 ARTEX 网页“系统 / Agent 接入”中核对命令输出的设备指纹、名称和申请时间，并勾选设备所需权限。然后运行：

```text
node scripts/artex-agent.mjs auth status
node scripts/artex-agent.mjs tasks list
node scripts/artex-agent.mjs tasks get --id <任务ID>
node scripts/artex-agent.mjs tasks create --description "检查已授权测试环境" --goal "只检查目标 example.test 的可用性"
node scripts/artex-agent.mjs skills list
node scripts/artex-agent.mjs skills upload --file ./example-skill.zip
```

创建任务会复用网页的现有任务启动流程。复杂任务使用 `tasks create --file task.json`，JSON 字段与现有任务创建接口一致。
上传复用现有 ZIP 校验和原子安装流程，默认拒绝同名覆盖；明确需要时使用 `--overwrite`。

可将 `integrations/local-agent/artex-agent` 文件夹复制到本机 Agent 的 Skill 目录，例如 `~/.codex/skills/artex-agent` 或 `~/.agents/skills/artex-agent`。
该文件夹包含完整 `SKILL.md` 和客户端脚本，复制后独立使用，不依赖 ARTEX 源码目录。

## 身份与权限

每台设备单独生成 Ed25519 身份，私钥只留在本机。Windows 使用当前用户 DPAPI 加密保存；其他平台使用用户私有目录及 0600 文件权限。
默认目录在 Windows 为 `%LOCALAPPDATA%/ARTEX/local-agent`，其他平台为 `$XDG_CONFIG_HOME/artex/local-agent`（未设置时为 `~/.config/artex/local-agent`）。
通过 `ARTEX_AGENT_CONFIG_DIR` 可以为不同 ARTEX 部署选择独立身份目录。身份已存在时登录命令不会覆盖。

网页分别审批 `tasks.read`、`tasks.create`、`skills.read`、`skills.upload`。未获批、权限不匹配或已撤销的设备会由服务端拒绝。
网页撤销立即阻止后续业务请求。请勿读取、展示或分享身份文件。

请求签名包含 HTTP 方法、完整请求路径与查询参数、秒级时间戳、随机 nonce 和 SHA-256 正文摘要。
服务端接受五分钟时间窗口并在数据库去重 nonce，防止跨实例重放。客户端拒绝跨站重定向和非回环 HTTP。
配对申请也需私钥证明；管理员应核对 CLI 显示的 SHA-256 公钥指纹，不凭设备名称批准。

创建任务和上传 Skill 会先写入 `local_agent_audit` 审计记录，再执行现有业务流程，并保存 HTTP 结果码；记录不包含任务正文、Skill 文件内容或私钥。进程中断留下未完成记录时，应先核对业务结果。

写请求因断网或超时而没有收到明确响应时，先核对任务或 Skill 是否已创建。客户端不会自动重试写请求，避免重复创建和不确定覆盖。

## 本地验证

```text
node --test --test-timeout=60000 scripts/artex-agent.test.mjs
```
