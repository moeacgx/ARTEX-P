# 在线目录

Skills、工具和 Agent 页面读取同一份 GitHub 仓库目录。默认来源为本仓库的 `dev` 分支、`catalog` 目录；也可以在页面设置自己的公开 GitHub 仓库、分支或标签及目录。仓库内容经 PR 更新后，用户在页面重新拉取即可获得新版本，无需重新发布 ARTEX。

目录只负责分发定义和文件。内置 Go 工具处理器的修改仍需程序发版；模型渠道、API Key、代理及其他实例配置不应提交到目录仓库。

## 使用

1. 在 Skills、工具或 Agent 页打开“在线目录”，检查仓库来源后拉取清单。
2. 选择条目，查看版本、许可、文件和安装预览。安装固定到本次预览的 Git 提交，并校验所有文件的 SHA256。
3. 安装技能与所需工具后，再安装依赖它们的 Agent 模板。缺少依赖时先安装依赖；目录不会自动执行远程代码。
4. 新工具和新 Agent 默认禁用。在原管理页面检查定义，配置模型、工具参数及绑定后再启用。
5. 以后拉取清单可检查更新。本地内容发生改动时停止覆盖，保留用户修改。更新前保留上一版本，用目录的回滚操作恢复。

回滚恢复上一次安装前的文件或模板字段，保留实例的凭据、启用状态和绑定。Agent 更新时增补的依赖绑定在回滚后仍保留，避免误删用户配置；首次安装没有旧版本可回滚。技能备份存放在隔离的 `.catalog-state` 目录，不会作为技能加载。

公开技能包可直接通过 GitHub 下载；应用内的 ZIP 下载来自所选仓库提交，不包含实例本地的配置或凭据。

## 维护目录

```text
catalog/
├── catalog.json
├── skills/<技能名>/SKILL.md
├── tools/<工具key>/tool.json
└── agents/<Agent key>/agent.json
```

`catalog.json` 包含 `schema_version: 1` 和 `items` 数组。每项必填 `kind`（`skill`、`tool`、`agent`）、`id`、`name`、`description`、`version`、`license`、`files`。`files` 是包内相对路径到文件原始字节 SHA256 的映射，目录按固定的 `skills`、`tools`、`agents` 路径读取。

技能的 `SKILL.md` 前置信息 `name` 必须与条目 `id` 一致。工具 `tool.json` 使用现有自定义工具的 `key`、`description`、`kind`、`schema`、`exec`、`deferred` 字段，不分发启用状态和 Agent 绑定。Agent `agent.json` 只包含 `key`、`name`、`description`、`prompt`；清单中使用 `tools` 和 `skills` 数组声明依赖。Agent key 和工具 key 使用小写字母开头及小写字母、数字、下划线。

例如，更新 `evidence-summary` 时先修改技能文件、递增清单版本，再重新计算 SHA256。提交前检查清单的每个哈希都与 Git 中文件字节一致。不要使用绝对路径、`..`、链接或指向任意网站的下载地址。目录源只读，网页不向 GitHub 写入内容或保存 GitHub Token。

在仓库根目录运行 `node scripts/check-catalog.mjs --write` 更新摘要，再运行 `node scripts/check-catalog.mjs` 核验。目录文件的换行固定为 LF；文件未列入 `files` 时不会随包下载。

本目录的示例采用仓库根目录的许可证；`catalog_utc_clock` 仅返回当前 UTC 时间，需要实例已配置 Python 解释器。`evidence_assistant` 依赖该工具及 `evidence-summary` 技能。
