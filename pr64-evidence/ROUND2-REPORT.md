# PR64 第二轮修复及验证记录

日期：2026-11-16（Asia/Singapore）；实现工人 cf0620df-dd40-45d8-b872-2220561006c6；任务 task_75e9af687981；dispatch ctx_da8ccd3d8ada。

本轮两项 Important 与两项测试夹具 Minor 已实现并验证，可交独立复审。本记录不构成独立审查通过或合并许可；服务端仍有三项已由独立审查在纯 dev 复现的失败，两项测试跳过。

## 版本和范围

- 本轮起点、上次复审 HEAD：e7d1b9cdfcf2e5855674c3690c577acece6404b3。
- 本轮代码修复提交及精确受测源码版本：b3e8aad6157e9d68ea764bdd0f38093b26109fe4；5 文件，188 插入、9 删除。
- 本报告作为随后独立文档提交；最终分支 HEAD 以协调者收到的 worker_done 回执为准，代码仍精确对应上面的修复提交。
- 差异：`git diff e7d1b9cdfcf2e5855674c3690c577acece6404b3 b3e8aad6157e9d68ea764bdd0f38093b26109fe4`。
- 未改审查报告、审查者工作树、UI 工作树、norma、前端和锁文件；未推送、合并、部署或读取生产凭据。
- 主目录 D:/脚本程序/开源程序二开/ARTEX-P 原两处用户修改仍保留：web/src/app/(main)/function/tasks/page.tsx、web/src/app/(main)/system/notify/_components/channel-form.tsx。

## 根因及修复位置

以下路径均为本任务工作树，行号对应上述代码修复提交。

1. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/task_proxy_switch.go:595、644、673：记录已无 advisory ShareLock 的 owner 引用数，仅从原 active_count 扣除该数，不再按剩余 owner 求和覆盖。未知差额保留，准入时既有一致性检查仍返回 ErrProxySwitchRecovery；非法非正 owner 数或无法安全扣减时同样保持恢复屏障。启动修复可提交已证实死亡的清理，但不会把未知引用洗掉。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/task_activity_recovery_test.go:138：unknown_only、live_and_unknown、dead_and_unknown、live_dead_and_unknown 四个子场景；真实 PG advisory-lock 会话继续占用时人工恢复锁不可获得，真实操作释放后经 ClearStaleTaskOperations 才能再次准入。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/task_proxy_switch_test.go:337、340：原 fail-closed 断言原样保留，现通过。
2. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/db.go:139：有 context deadline 的调用清除包装层预设 socket deadline，由 pgx CancelRequestContextWatcherHandler 独占取消后的 socket 期限，保留 100ms CancelRequestDelay、3s DeadlineDelay；没有 context deadline 的原 30s I/O 兜底不变。避免 50ms 预设期限在取消 handler 延长之前触发 I/O 错误。Query/Exec 原错误透传、IsValid 淘汰、pgx SafeToRetry 边界未改。
   - C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/postgres_cancel_test.go:10：原 50ms + pg_sleep(5) + 同一 checked-out conn SELECT 1 断言不变。
   - 同文件 :37：真实 pg_terminate_backend，验证故障会话报错并淘汰、池提供不同 backend PID 的新连接。
   - 同文件 :77：QueryRow 超时后核对原 backend PID；归池再次借用仍是原 PID，验证恢复而非偷偷替换会话。
3. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/db/task_activity_recovery_test.go:229：启动修复用例先通过 GetProxyPolicy 创建 policy 再 seed；保留操作计数、owner、活动三项清零断言，实际执行 RepairStaleTaskOperations。
4. C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/server/side_questions_test.go:500：通过 Manager.SetTaskStatus(running) 和 SetTaskPaused(true) 同步持久及内存状态，再走 restoreTaskRuntimes 并验证新 side checkpoint 发布；原 fixture 复用 Manager，单改数据库无法更新其已有 created 内存句柄。created 草稿不恢复行为仍由 TestPlannerPGRestartSkipsCreatedTask 验证。

## RED / GREEN

所有日志及本轮运行器均保留于 C:/Users/Administrator/orca/workspaces/ARTEX-P/pr64-review-fixes-20261116/pr64-evidence/；日志和二进制不纳入代码提交。

- RED：round2-red-TestTaskOperationUnknownReferencesRequireManualRecovery.log，4/4 子场景失败，未知引用被归零或减至仅 live owner。
- RED：round2-red-TestPostgresCancelKeepsCheckedOutConnectionUsable.log，20 次中 19 PASS / 1 FAIL；原同连接 SELECT 1 报 driver: bad connection。
- RED：round2-red-TestRepairStaleTaskOperationsAtStartup.log，修复前夹具 sql: no rows。
- side 夹具直接只改 PG 的中间尝试仍因内存 created 未同步失败，诊断保留 round2-focused-server-TestSideRestoredWorkerRuntimePublishesNewCheckpoint.log；改为 Manager 同步接口后通过，最终全量也通过。
- 最终源码取消重复：round2-cancel-final-db-summary.json，Exec 同连接复用 100 PASS / 0 FAIL / 0 SKIP；Query 原 PID 及归池复用 100 PASS / 0 FAIL / 0 SKIP。每个批次 -test.count=100、-test.timeout=60s，约37秒；未加长原50ms或pg_sleep(5)。
- 最终源码连接重复：round2-connection-final-db-summary.json，真实 backend 终止淘汰 10/10；真实 PG INSERT RETURNING 提交后 EOF 不重放 10/10；database/sql 执行次数仅一次 10/10。
- 写入响应丢失仍为真实 PG 提交后在驱动边界注入 EOF，不是 TCP 物理断包实验。

## 最终受影响包验证

使用原专用合成 PG 容器 artex-pr64-074daa-pg-v2，127.0.0.1:25432；仅其中 artex_proxy_verify_test 库。运行前核对没有其他测试会话，每个顶层测试串行重建专用库；未操作原共享 artex-mcp-db-20261115。测试安全白名单未放宽。Linux amd64、CGO_ENABLED=0 测试二进制由本工作树重编译，每项 -test.timeout=60s，唯一运行器串行持有该库。

- db：275 顶层测试，275 PASS / 0 FAIL / 0 SKIP，round2-full-db-summary.json；较上轮增加3项顶层回归。
- server：483 顶层测试，478 PASS / 3 FAIL / 2 SKIP，round2-full-corrected-server-summary.json。
- integrations/xiequ：5 PASS / 0 FAIL / 0 SKIP，Windows 原生 go test -v -count=1 -timeout=60s ./integrations/xiequ，round2-xiequ-final.log。
- go vet ./db ./server ./integrations/xiequ：FAIL，唯一报告 server/channel_proxy_reference_test.go:73 复制含 sync.RWMutex 的 Task；与独立复审的纯 dev 问题一致。round2-vet.log。
- git diff --check：PASS。
- 原三项 Important 回归继续通过：首 worker 退休、活跃多 worker 缩容及 pause/delete 真退出、Xiequ 跨身份在途与配额、十项 TestRetryingPostgres*、真实提交错误不重放均包含在本轮全量及重复测试中。
- MCP、WARP、迁移、真实进程退出等 server 全量用例均实际执行；例外仅下列已登记失败及跳过。
- norma 与前端本轮未修改，按本轮限定不重复无关全量；上轮 norma 679 PASS / 8 FAIL / 3 SKIP，独立复审已在纯 dev 与原 PR 逐项复现8项失败。前端 npm ci、静态构建、TS 的通过证据沿用 REPORT.md，不能表述为本轮重跑。

测试运行器第一次 server 全量误用 /workspace 而非 /workspace/server，导致 ../db/schema.sql 缺失；该批次已停止，round2-full-server-* 仅作诊断，不计最终。修正为包目录后从头跑完483项，最终只用 round2-full-corrected-server-*。一次 integrations/xiequ 在 Windows 误执行 Linux 交叉编译目标报 Win32 格式错误，也不计最终；清除 GOOS/GOARCH 后原生5项全通过。无源码补丁掩盖环境错误。

二进制 SHA256：

- round2-db.test：088FCA76C23EDA76BAC98EF7068243B56F4991B1731AFF22E338804333203055。
- round2-server.test：F49EB24206995B9494A751CAA476F02F9A63D33E65C2EF6782E05591661053F8。

## 仍失败或跳过的项目

独立复审报告只读位置：C:/Users/Administrator/AppData/Local/Temp/artex-pr64-rereview-c3141914/INDEPENDENT-REVIEW.md；该报告逐项对照纯 dev / 原 PR / 融合 / 修复，以下三项已在纯 dev 复现，本轮未改动其源码或断言。

- FAIL TestRetestHTTPThroughConversationAndTools：finding_retests_test.go:194，provider not called。
- FAIL TestRetestFixedUpdatesFindingThroughConversation：finding_retests_test.go:280，finding 仍为 pending。
- FAIL TestProxyPGLegacyMigrationAndWarpDTO：proxy_integration_test.go:397，停止 WARP 后状态断言不符；测试未打印实际状态，不能据文字推定为返回200或生产虚报healthy。
- SKIP TestLiveContextReview：未设置 ARTEX_REVIEW_LIVE_CONFIG；未为测试读取生产模型配置。
- SKIP TestUpdateOfferRequiresPersistentDockerLauncher：fixture 未识别持久容器启动环境。

每项本轮完整输出分别在 round2-full-corrected-server-测试名.log。norma八项及vet既有缺陷仍存在，不声称全仓全绿；未知活动引用仍需人工确认后恢复是预期保护。第二轮修复已经具备交独立审查的代码及真实隔离PG证据，后续是否合并由独立审查者和协调者决定。
