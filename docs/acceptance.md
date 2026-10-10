# P10.3 整体验收与交付

本地验收使用真实 MongoDB 单节点副本集、Gin/JWT、ADK、OpenAI/MCP/A2A 官方 SDK 和受控本地 HTTP 提供方。外部提供方、生产凭据与多节点部署故障仍待验证；本地通过不代表项目全部验收完成。

2026-09-28 已执行下述完整脚本：全量测试、10 个关键包 race、vet、build 均通过，无数据库集成跳过；仅真实模型/外部工具两项冒烟因未配置而跳过。原始 JSON 证据位于 `.local/acceptance/tests.jsonl`，详细交接见 [实施进度](implementation-plan.md)。

## 可重复检查

环境：Windows amd64、Go 1.27.1、MongoDB 8.0.32；race 需要启用 CGO 并安装可用 C 编译器。依赖版本以 go.mod/go.sum 为准。测试启动临时 MongoDB、初始化单节点副本集并清理自己的临时数据，不修改业务数据库。

在仓库根目录执行（MongoDB 路径可替换）：

```powershell
$env:CAGENT_TEST_MONGOD = (Resolve-Path '.local/mongodb/mongodb-win32-x86_64-windows-8.0.32/bin/mongod.exe').Path
./scripts/acceptance.ps1
```

普通集成测试也可指定专用 `CAGENT_TEST_MONGODB_URI`，不能指向生产集群；故障注入只允许使用脚本要求的 `CAGENT_TEST_MONGOD` 临时实例。两个变量同时存在时临时 mongod 优先。脚本运行全量 test、关键包 race、vet、build，遇失败立即停止；除两个显式外部冒烟外，任何 skip 都使验收失败。JSON 测试证据保存在 `.local/acceptance/tests.jsonl`，不会提交到仓库。`-SkipRace` 仅供工具链不支持时诊断，不能用于声称并发验收完成。其他平台可执行下面的 Go 命令，设置对应的 mongod 路径。

```sh
go test ./... -count=1 -timeout=240s
go test -race ./internal/observability ./internal/config ./internal/app ./internal/adapter/adk ./internal/adapter/mongodb ./internal/adapter/a2a ./internal/adapter/mcp ./internal/tool ./internal/transport/httpapi ./internal/bootstrap -count=1 -timeout=300s
go vet ./...
go build ./...
```

## 行为证据与故障复现

下表测试均可通过 `go test <包路径> -run '<测试名>' -count=1 -v` 单独复现；数据库环境变量仍须设置。不要将测试中的 failpoint 用于业务环境。

| 范围 | 包路径与测试名 | 判定依据 |
| --- | --- | --- |
| 完整交付链路 | `./internal/transport/httpapi`：`TestAcceptanceTaskRestartReplay` | JWT→会话→Run→模型工具调用→A2A 任务→观察 HTTP 503→关闭旧应用→重建客户端/应用→扫描恢复→原调用结果接纳→模型完成→SSE 排他重放；工具执行一次、模型两次、交付 applied；重启后幂等和跨租户/用户隔离 |
| HTTP 断连与取消 | 同包：`TestHTTPADKDatabaseLifecycle` | SSE 断连后运行完成、跨 Scope 拒绝、取消持久化、旧游标 410 |
| 双实例恢复及取消竞争 | `./internal/app`：`TestTaskRestartCompetitionAndCancel` | 独立数据库客户端竞争同一检查点；每任务只消费一次；取消 Run 后迟到成功 discarded，不生成 |
| 部分交接故障 | 同包：`TestPartialHandoffRepairIncludesCancelledRun` | 整批句柄仅部分登记后恢复补齐；取消也保留远端事实，不重执行工具 |
| 不确定生成恢复 | 同包：`TestCloseAndRecoveryDoNotReexecuteStartedRun` | 已开始但没有安全检查点的运行明确失败，不盲目重调 |
| 事务回滚 | `./internal/adapter/mongodb`：`TestMongoRollbackStartAndCommit`、`TestMongoCheckpointUpdateFailureRollsBackTrack` | 故障后无部分消息、事件、占用或任务记录 |
| 未知提交与请求取消 | 同包：`TestMongoUncertainCommitAndContextCancellation` | commitTransaction failpoint 返回未知提交结果，回执重放无重复 |
| 提交连接故障 | 同包：`TestMongoConnectionLossDuringCommit` | 服务端在 commitTransaction 断开 TCP，驱动重连提交；回执、Run 版本和单一事件序号一致，确认 failpoint 恰好触发一次 |
| 跨连接取消/消费 | 同包：`TestMongoCancelVersusApplyAcrossClients` | 取消与 Apply 竞争保持单一一致终态 |
| 租约接管 | 同包：`TestMongoLeaseExpiryReleaseAndTakeover` | 到期接管递增 Fence，拒绝旧写入 |

新端到端测试的“重启”是关闭 Application 后重建应用、Catalog 和独立 MongoDB 客户端，并非 OS 强杀；专项故障测试覆盖持久化中间状态。多节点选举、真实 TCP 网络分区和进程强杀仍须部署环境验收，不能从单节点测试推断。

## 外部验收门槛

模型冒烟：设置 `CAGENT_TEST_MODEL=1` 及 MongoDB 模型配置，执行 `go test ./internal/adapter/adk -run TestRealOpenAISmoke -count=1 -v`。接口兼容性及摘要质量必须按目标提供方核对，见 [模型](adk.md) 与 [上下文](context.md)。

工具冒烟：按 [工具说明](tools.md) 设置 `CAGENT_TEST_TOOLS=1`、`CAGENT_TEST_TOOLS_MONGODB_URI`、`CAGENT_TEST_TOOLS_MONGODB_DATABASE` 和 `CAGENT_TEST_TOOL_CALL`，执行 `go test ./internal/bootstrap -run TestExternalToolSmoke -count=1 -v`。远端连接从指定数据库的 `tool_connections` 读取。该测试真实执行一次指定工具，选择专用测试账户与可控操作。仅返回句柄不算长任务通过。

外部长任务需另外通过实际 HTTP API 完成：提交会调用目标 A2A 工具的 Run；记录 SSE 中本地 task_id 和最后序号；确认等待后重启服务；观察同一远端任务被查询且启动次数不增；完成后确认原 ToolCall 结果、单一终止事件与断点重放。分别验证暂停补充输入、授权引用轮转、取消与完成竞争。MCP 当前 SDK 不支持 tasks，不能把该能力列为已交付。

发布记录必须包含版本/提交、Go/MongoDB 版本、提供方与协议版本（不含凭据）、命令与结果、跳过项、故障步骤及恢复结果。目标部署还需验证多节点选举/网络中断、MongoDB 认证/TLS、备份恢复及容量目标。所有范围内功能和真实外部集成通过前，项目保持“本地验收完成，外部验收待验证”。

部署步骤及排障见 [部署交付](deployment.md)。
