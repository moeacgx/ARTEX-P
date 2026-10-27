---
name: artex-agent
description: 通过本机 Ed25519 设备身份接入 ARTEX，查询任务和 Skill、创建任务、上传 Skill ZIP；首次设备须在 ARTEX 网页核对指纹并审批权限，无需管理员密码或平台 Token。
---

# ARTEX 本地 Agent

使用本 Skill 的 `scripts/artex-agent.mjs`，要求 Node.js 20 或更高版本，无需安装 npm 包。

先调用 `auth status`。首次接入调用：

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
node <脚本> skills list
node <脚本> skills upload --file <本地Skill.zip>
```

- `tasks.read` 允许查询任务，`tasks.create` 允许创建任务。
- `skills.read` 允许查询 Skill，`skills.upload` 允许上传 Skill ZIP。
- 创建任务复用 ARTEX 的现有创建和启动流程，会开始规划与执行；仅在用户要求执行该任务且目标授权范围明确时创建。
- 任务 JSON 使用 ARTEX 现有创建接口字段，例如 `description`、`goal`、`name`、`source_task_ids`、`proxy_mode`、`proxy_id`，不编造资源 ID。代理和模型配置由 ARTEX 原有校验处理。
- Skill ZIP 内应包含 `SKILL.md` 和所需文件，压缩包须小于 20 MiB；默认遇到同名 Skill 返回冲突。仅在用户明确要求覆盖时添加 `--overwrite`。
- 写请求网络中断后，脚本不会自动重放。先通过网页或列表确认任务、Skill 是否已创建，再决定是否重试。

Windows 私钥使用当前用户 DPAPI 加密保存；其他系统保存于当前用户私有目录，权限为 0600。
不读取或打印身份文件、私钥或管理员凭据。仅记录公开设备 ID、指纹、授权状态和必要业务结果。
HTTP 仅允许本机回环地址，其他部署使用 HTTPS。需要多个独立身份时设置 `ARTEX_AGENT_CONFIG_DIR`。
撤销访问在 ARTEX 网页完成；本机删除身份文件不等于服务端撤销设备。
