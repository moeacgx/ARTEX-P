# PR64 三项 Important 修复交接

日期：2026-11-16（Asia/Singapore）。实现工人：cf0620df-dd40-45d8-b872-2220561006c6；任务：task_074daa863021。

## 结论与提交

三项已知 Important 已修复并完成 RED/GREEN、真实隔离 PostgreSQL 和活跃执行回归。本报告是实现及验证记录，不是独立审查通过结论；全部回归仍有基线失败，不能据此宣称可直接合并。

- 融合基线：fab8a675bc26ff58a231369b422d645e6baf8a47。
- 基线父提交：origin/dev 8322fa79aeea574581ff1231adcb0f9c936f4029；PR64 24fe7c9c1720fa497f579bb22e389027e5b5bf98。
- 修复提交：1baafb0279e073cdc35a4fadee45080704476c48；10 文件，595 插入、16 删除。
- 查看纯修复：git diff fab8a675bc26ff58a231369b422d645e6baf8a47 1baafb0279e073cdc35a4fadee45080704476c48。
- 未推送、未创建/合并 GitHub PR、未部署，未读取生产凭据。

## 修复位置与行为

以下均是本次工作树中的完整路径，行号为修复提交版本。

1. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/db.go:188、207、263：Query/Exec 原样返回驱动错误，连接故障通过 invalid + IsValid 淘汰，不扩张 pgx SafeToRetry。真实业务 INSERT RETURNING 位于同根目录 db/config.go:1017。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/postgres_query_replay_test.go:57、112：SQL 层记录提交次数；真实 PG 提交一条后向 database/sql 注入 EOF，验证仅一条、后续新连接可用。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/postgres_connect_retry_test.go:146、159：故障连接淘汰且原错误保留；pgx 自己给出的 ErrBadConn 仍透传。
2. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/engine.go:417：共享 execCtx 绑定 taskRuntime.ctx。退休 worker 不再取消共享执行，当前操作排空后退出领取循环；任务暂停/删除仍取消真实操作。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/engine_context_retirement_test.go:14、44：首 worker 创建共享上下文、扩容至三个真实模型调用、缩容后全部排空、剩余 worker 领取下一意图，pause/delete 等待模型真正退出及数据库活动释放。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/task_admission_test.go:293：原 fixture 故意取消服务根上下文以阻止真实执行；相应断言改为继承 runtime 取消。真实存活父上下文由上述活跃执行测试覆盖，不能从已取消根上下文创建新执行。
3. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/xiequ_settings.go:337、353、384、420：状态绑定规范化 UID + BaseURL 的摘要和随机配置修订；身份/UKEY 变化在配置事务内清空状态，A→B→A 也拒绝旧刷新；无身份历史状态不认领。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/xiequ_service.go:71：使用捕获配置过滤读取状态，状态写回必须通过配置行锁和修订检查。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/xiequ_identity_test.go:8、30、56；C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/xiequ_identity_integration_test.go:17：两账号共享同一 IP、新账号配额满及旧刷新跨账号完成，验证手工 IP 不删除、新状态不覆盖。

## 验证环境和证据

专用容器 artex-pr64-074daa-pg-v2（PostgreSQL18，host 127.0.0.1:25432），仅本任务使用。库 artex_proxy_verify_test 位于这个独立物理实例，名称沿用原 proxy 测试白名单；未放宽生产保护。原 artex-mcp-db-20261115 仅检查可用性，未改其库。合成测试角色 artex_test，未使用生产凭据。

每个 db/server 顶层用例前在专用容器串行 dropdb/createdb，同一时刻只有一个用例访问此库；每用例 -test.timeout=60s。最初同库分批运行发现测试密钥/数据污染，随后全部改用逐用例独立重建，其旧日志只保留诊断，不计最终统计。Linux amd64 测试二进制由本工作树 Go 交叉编译；/workspace 是融合基线 fixture，/tests/*.test 是修复后代码。norma 在 Linux 非 root 用户下逐用例运行，补齐 bash、node、rg 和 /tmp/norma 写权限后复测环境失败。

全部日志在 C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/pr64-evidence/。

| 范围 | PASS | FAIL | SKIP | 说明 |
| --- | ---: | ---: | ---: | --- |
| db | 271 | 1 | 0 | 272 顶层用例，含新增 SafeToRetry 回归 |
| server | 476 | 5 | 2 | 483 顶层用例，环境与断言修正后统计 |
| norma | 679 | 8 | 3 | 690 顶层用例；本次没有修改 norma |
| integrations/xiequ | 全包通过 | 0 | 0 | go test -count=1 -timeout=60s |
| 最终三项修复定向 | 8 | 0 | 0 | 独立真实 PG 重跑，另两项驱动单测通过 |

- RED：red-context-query.log（提交被重放3次、退休首worker取消共享ctx）；red-xiequ.log（新账号继承旧 ManagedIPs）。
- GREEN：final-green-summary.json、final-green-*.log；驱动边界另见 final-db-TestRetryingPostgresConn*.log。
- 全量明细：isolated-summary.json、verification-summary.json、norma-linux-summary.json、norma-rerun-summary.json；最终合并统计 final-summary.json。
- MCP 协议/业务桥、LocalAgent CLI（安装 Node 后）、WARP 生命周期/健康屏障、代理迁移删除、MCP 多传输排空相关用例通过；LegacyMigrationAndWarpDTO 的健康断言仍失败，详见下方基线对照。
- 前端：原 lockfile 的 npm ci 退出0；NEXT_EXPORT=1 NEXT_PUBLIC_MOCK=0 npx next build --webpack 退出0；npx tsc --noEmit 退出0。证据 frontend-npm-ci-full.log、frontend-build-final.log、frontend-typescript-final.log。lockfile 无变更；npm 提示 sharp install script 未列 allowScripts，但实际静态构建成功；初次审计报告现有26项依赖漏洞，未擅自升级依赖。
- git diff --check 通过；go vet ./db ./server ./integrations/xiequ 在 server/channel_proxy_reference_test.go:73 报复制含锁 Task，未修复融合基线同样失败（baseline-vet.log）。

复现单个数据库用例（只允许上述专用容器；二进制与日志均保留）：

~~~sh
docker exec artex-pr64-074daa-pg-v2 sh -c 'dropdb -h 127.0.0.1 -p 25432 -U artex_test --force artex_proxy_verify_test && createdb -h 127.0.0.1 -p 25432 -U artex_test artex_proxy_verify_test'
docker exec -w /workspace/server -e ARTEX_PG_DSN=postgres://artex_test@127.0.0.1:25432/artex_proxy_verify_test?sslmode=disable artex-pr64-074daa-pg-v2 /tests/server.test -test.run '^TestActiveWorkersScaleDownDrainAndTaskControlExit$' -test.count=1 -test.timeout=60s -test.v
~~~

涉及门禁的全量测试另设置 ARTEX_LOCAL_AGENT_TEST_DSN、ARTEX_WARP_HEALTH_TEST_DSN 为相同 DSN，以及 ARTEX_PG_STABILITY_TEST=1。基线对照二进制 /tests/baseline-db.test、/tests/baseline-server.test 从精确融合提交归档编译。

## 未解决的基线与环境问题

以下六项均在未修复融合基线和最终版本、每用例新库中复现；对应 baseline-<包>-<测试>.log / final-<包>-<测试>.log，可逐一比较：

- db TestRepairStaleTaskOperationsAtStartup：sql no rows（task_activity_recovery_test.go:140）。
- server TestRetestHTTPThroughConversationAndTools：provider not called。
- server TestRetestFixedUpdatesFindingThroughConversation：复测后 finding 仍为 pending，未更新 fixed。
- server TestProxyPGLegacyMigrationAndWarpDTO：停止 WARP 后健康 DTO 断言失败。
- server TestSideRestoredWorkerRuntimePublishesNewCheckpoint：restored task runtime missing。
- server TestProxyPGTaskSwitchClearsOnlyStaleOperations：残留活动 fail-closed 断言失败。

norma 八项失败：llm/TestDoStreamHonorsConfiguredRetries（nil HTTP client panic）；noa/TestAdaptiveGrowthTracksTheWindow、TestSuppressionReleaseFitsInsideTheWindow；noaadapter/TestMostlyCompetentModelStaysBounded、TestLongSessionStaysBounded、TestLongSessionWithPartiallyCompliantModel、TestOverflowLearningIsWiredIn、TestTierTwoIsUnreachableOnSmallWindows。该目录与融合基线完全相同，未将无关修复纳入本提交。Windows 全包结果也保留，最终以 Linux 逐用例环境修正后的统计为准。

跳过：server 真实模型配置未提供；旧 Docker launcher 检测在测试 fixture 下未识别容器。norma 两项缺外部 acp-kernel checkout，一项本轮未形成 tier-2 block。没有为消除 SKIP 读取生产配置。

## 边界和下一步

- PG 回归使用真实提交和驱动边界 EOF 故障注入，并非真实 TCP 断链；不宣称经过网络代理断流验证。
- Xiequ 配置事务不跨网络持锁。旧在途请求继续使用旧账号快照，可能在旧账号完成外部操作；它不能删新账号记录，也不能写回新修订状态。配置切换不承诺撤回已经发出的旧账号请求。
- 缩容选择退休 worker 仍沿用原行为，当前意图排空；pause/delete 已验证模型真实退出与持久活动释放。未运行 race detector。
- 主目录两个用户修改仍保留：web/src/app/(main)/function/tasks/page.tsx、web/src/app/(main)/system/notify/_components/channel-form.tsx。
- 等待原独立审查工人 ctx_ee18ed82c237 复审；协调者需评估上述融合基线失败后决定合并，不应标记全绿或线上验收成功。
