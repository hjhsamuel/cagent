# 完整实施计划与进度

最后更新：2026-09-28。

本文件记录实施顺序、验收要求和跨会话交接信息；架构约束见 [architecture.md](architecture.md)。存在类型或接口不代表功能已完成。状态以实际代码与验证结果为准，不按文件数量推算完成百分比。

## 当前进度与下一个任务

- 已完成：项目骨架、领域类型与接口契约；P1.1 领域身份与关联校验；P1.2 公共错误约定；P1.3 配置；P1.4 日志（Logrus）；P1.5 领域状态规则。P1 公共组件全部完成。
- 已完成：P2 存储契约补齐；P3 MongoDB 适配、索引、事务、租约、回执、恢复管理与真实副本集集成测试。
- P3 简化已完成：移除 store 全部仓储/事务接口与转发对象，直接调用 `Database` 具体方法；移除泛型读写及事务封装，方法内直接操作 MongoDB collection。
- 已完成：P4 会话/Run 应用服务、独立运行生命周期、取消、持久事件轮询与恢复接入点。
- 已完成：P5 上下文组装、摘要覆盖/工具配对校验、可替换计数器与输入预算、P4 准备接入。
- P6 代码与本地验收已完成：ADK/OpenAI 兼容执行、动态模型配置、预算、持久化输出与完成检查点恢复。真实提供方冒烟待运行环境配置，不能据此标为全部验收完成。
- P7 已完成：Gin HTTP、JWT 认证、DTO/错误/请求关联、SSE 重放/心跳/写超时、健康/就绪检查与完整启动/关闭装配。真实副本集和本地模型协议端到端验收通过。
- P8 代码与本地验收已完成：作用域工具注册、本地工具、MCP/A2A 客户端、ADK 工具循环、任务句柄与检查点交接、暂停交互路由。外部 MCP/A2A 提供方冒烟待配置，不标记为外部验收通过。
- P9 代码与本地验收已完成：任务观察/取消、分支精确续接、原子消费、部分交接补齐、启动恢复扫描与不确定副作用屏障。真实 MongoDB、ADK、本地模型协议及双实例恢复测试通过。
- 已完成：P10.1 阈值/近期轮次/摘要模型配置、保守用户要求保留、预算化摘要、有界回退及租约/CAS 快照保存；真实 MongoDB 与本地 OpenAI/ADK 链路通过。
- 已完成：P10.2 单实例容量、过载与幂等重放、固定类别指标和有界本地追踪、日志字段脱敏及恢复数据保留责任；真实副本集与关键并发行为验收通过。
- P10.3 已补齐整体端到端行为测试、真实提交连接故障注入、严格验收脚本及部署交付说明；本地验证记录见本节后文。P6 真实模型及 P8/P9 外部提供方仍待环境配置，项目不标记全部验收完成。
- 入口现已加载完整配置并装配 MongoDB、预算化 ADK/OpenAI 应用服务与 HTTP 监听；需要显式配置 JWT 与模型参数。
- 最近一次代码验证：2026-09-28，设置 CAGENT_TEST_MONGOD 后运行 `./scripts/acceptance.ps1` 通过：全量 `go test ./... -json -count=1 -timeout=240s`、10 个关键包 race（timeout=300s）、`go vet ./...`、`go build ./...` 均通过。Go 1.27.1 windows/amd64、MongoDB 8.0.32 真实临时单节点副本集；数据库集成无跳过，仅真实模型和外部工具冒烟待配置。证据位于 `.local/acceptance/tests.jsonl`。
- 待后续环境验证：多节点选举/网络分区、mongos、生产认证/TLS、真实模型、MCP/A2A 提供方；HTTP/SSE 本地端到端链路已验证。单节点副本集事务与独立客户端并发已验证。

## 跨会话执行方式

1. 先读取本文件、架构文档、目标模块源码及适用的 AGENTS.md；核对工作区已有修改。
2. 默认从第一个未完成项开始；用户指定阶段时先检查其前置依赖。每次仅完成当次授权范围，不因本计划存在而自动实施所有阶段。
3. 实现前记录本模块接口与验收边界；必要的接口调整同步更新调用方和架构文档。
4. 新增、修改的公共接口及复杂逻辑补充详细中文注释，解释职责、约束、关联关系、并发和故障语义。
5. 编写有效的行为测试，执行相关测试以及全量编译、静态检查。集成环境不可用时明确记录，不把模拟测试当作真实集成验证。
6. 完成后更新本文件中的状态、变更文件、验证命令及结果、未解决问题和下一项。部分完成不能标记为完成。

新会话可使用：

> 请先阅读 docs/implementation-plan.md 和 docs/architecture.md，核对当前代码，从第一个未完成项继续实现本次指定的模块。补充详细中文代码注释和行为测试，完成后更新实施进度。

## 固定架构边界

- 模块化单体，唯一服务入口；不增加独立工具 Worker 或工具执行队列。
- domain 不依赖 HTTP、MongoDB、ADK 或协议 SDK；存储直接调用 `*mongodb.Database` 的具体方法，`store` 不定义 interface；其他适配器承担外部系统转换。
- Scope 包含可信 TenantID 和 UserID；全部资源访问受作用域约束，不允许空 Scope 全量查询。
- 同会话一个活动 Run，通过数据库原子占用、租约及版本约束实现；不依赖进程内锁保证多实例一致性。
- HTTP 断开只停止订阅；运行及工具任务跟踪独立于请求生命周期。
- 事件先持久化后发布，序号在 Run 内单调递增，重放与实时订阅之间不能丢事件。
- 长任务由工具实际返回的句柄决定；跟踪已有远端任务，不因观察失败重新执行工具。
- 任务结果按 Scope、Run、InvocationID、ToolCall ID 精确续接；任务终态、检查点和消费标记须可靠协调。
- 领域类型不绑定 BSON/JSON 标签；适配层定义文档和 DTO。
- 原始历史保留；摘要是派生数据。工具调用/结果对和未完成调用必须保留。

## 阶段总览

| 阶段 | 模块 | 状态 | 完成标志 |
| --- | --- | --- | --- |
| P0 | 骨架与架构 | 已完成 | 类型、接口与唯一入口可编译 |
| P1 | 公共组件 | 已完成：5/5 子项完成 | 校验、错误、配置、日志、状态规则可复用 |
| P2 | 存储契约补齐 | 已完成 | 事务、租约、幂等与恢复边界明确 |
| P3 | MongoDB 适配 | 已完成 | Database 具体方法、索引及并发约束通过真实副本集集成测试 |
| P4 | 应用服务与事件流 | 已完成 | 会话、Run、取消及持久化事件流可用，真实副本集与运行时替身验收通过 |
| P5 | 上下文基础能力 | 已完成 | 系统/摘要/历史组装、工具配对与预算校验通过，超预算不调用运行时 |
| P6 | ADK 最小执行链路 | 代码与本地验收完成，真实模型验收待配置 | 输入到模型再到持久化输出贯通 |
| P7 | HTTP 与启动装配 | 已完成 | JWT、HTTP/SSE、真实副本集端到端与启动/关闭验收通过 |
| P8 | 工具注册与协议适配 | 代码与本地验收完成，外部提供方验收待配置 | 本地/MCP/A2A 可调用，任务句柄可靠交接，暂停交互可用 |
| P9 | 长任务与恢复 | 代码与本地验收完成，外部提供方待配置 | 跟踪、取消、精确续接、原子消费、重启及多实例恢复已验证 |
| P10 | 压缩与运行保障 | P10.1/P10.2 已完成；P10.3 本地交付已实现，外部验收待验证 | 压缩、容量、观测及整体验收完成 |

## P1 公共组件

### P1.1 领域身份与关联校验 — 已完成

实现文件：

- `internal/domain/validation.go`：Scope、Session、Run、Message、Event、ContextSnapshot 校验及所属资源匹配。
- `internal/domain/tool_validation.go`：Agent 调用身份、工具协议、调用、句柄、结果、ToolOutcome 和 Task 的关联校验。
- `internal/domain/validation_test.go`：空字段、跨租户/用户、会话/运行关联、结果互斥、协议和嵌套任务关联测试。
- README 与架构文档已同步。

验收已通过：作用域不能为空；任务与嵌套调用作用域一致；Result/Task 恰有一个；结果 CallID 与原调用一致；句柄协议与调用一致；校验不改写 ID、不回显敏感字段值。

实现边界：校验方法需要调用方显式使用；不验证认证真实性、数据库引用存在性、状态迁移、版本、时间和消息内容。错误已在 P1.2 统一为参数无效类别，并保留结构化字段路径。

### P1.2 公共错误约定 — 已完成

实现文件：

- `internal/apperrors/errors.go`：仅依赖标准库，定义稳定 Kind 常量、带私有字段的 Error、New/Wrap、Is/Unwrap 和安全格式化，附详细中文契约注释。
- `internal/domain/validation.go`、`tool_validation.go`：所有校验失败统一为 `ErrInvalidArgument`；保留已有字段路径、首错顺序和引用对象的上下文包装，不改变校验规则。
- `internal/store/repositories.go`：`ErrNotFound`、`ErrConflict` 保留为公共类别的常量别名；现有名称与新类别可互相识别。
- `internal/tool/registry.go`：明确能力不支持及观察取消/超时的错误约定。
- `internal/apperrors/errors_test.go`、`internal/domain/errors_test.go`、`internal/store/errors_test.go`：公共错误、校验边界及兼容别名行为测试。
- README、架构文档及本文件同步更新。

验收已通过：

- 四类稳定错误：参数无效、资源不存在、版本冲突、能力不支持；经过多层 `%w` 包装仍可用 `errors.Is` 判断，用 `errors.As` 提取字段与安全说明。
- 原始错误身份和类型可沿原因链识别；`context.Canceled` 与 `context.DeadlineExceeded` 保持独立可识别。`Wrap` 的原因是 nil 时返回 nil。
- 错误文本及 `%v`、`%+v`、`%#v`、`%q` 等格式化不展开底层原因；测试验证凭据、提示词、连接信息不泄露。
- 领域缺失字段、关联不一致、自引用、未知协议、互斥结果、嵌套任务均返回参数无效；原 P1.1 测试全部通过。
- `go test ./...`、`go vet ./...`、`go build ./...` 均通过。

实现边界：字段与说明由创建者提供可信静态文本，本包不自动识别任意字符串中的秘密。显式 Unwrap/As 提取的底层错误仍是内部诊断数据，不得直接对外输出；HTTP DTO、状态码和 SDK 错误转换留给后续适配层。多层公共错误可能匹配多个类别，`errors.As` 提取的最外层 Error.Kind 表示当前边界分类。存储别名保留名称及类别判断兼容性，错误展示文本不保证旧文案兼容。没有新增认证、重试或任务状态推断规则。

未解决问题：本项无；真实外部系统错误转换及端到端验证属于后续适配阶段。下一项为 P1.3 配置。

### P1.3 配置 — 已完成

实现文件：

- `internal/config/config.go`：现有配置类型的详细中文注释与独立默认值，URI、模型提供方及模型名不设默认值。
- `internal/config/load.go`：Load/LoadFromEnv，P1.3 实现 16 个环境变量，P1.4 当前增加四个日志变量；解析时间与整数，失败返回零值 Config。
- `internal/config/validation.go`：集中校验 HTTP 监听地址、MongoDB/Agent 必填字段、正时间间隔及 Token 预算，错误使用 P1.2 的类别与字段路径。
- `internal/config/config_test.go`：默认值、完整覆盖、空值、缺失字段、非法类型/组合、预算边界/溢出、安全错误、真实进程环境读取及示例加载行为测试。
- `.env.example`、`docs/configuration.md`：无真实凭据的完整示例及配置规则；README、架构文档及本文件同步更新。

验收已通过：

- 默认值 < 已设置的环境变量；未设置保留默认值，显式空值覆盖后报错，合法零值不回填；不隐式裁剪或展开字符串。
- HTTP 支持空 host、ASCII 主机名、IPv4/IPv6，端口限定数字 1–65535；时间间隔必须为正；必填项拒绝纯空白。
- 输入预算至少剩余一个 Token，逐项扣减防止整数溢出；工具和安全预留允许为零。
- 错误可用 `errors.Is/As` 识别类别和配置路径；错误文本及原因链不包含原始输入，示例不包含真实密钥。
- `go test ./internal/config`、`go test ./...`、`go vet ./...`、`go build ./...` 均通过。

实现边界：配置包不自动读取 `.env`，不加载 SDK、不输出配置日志、不执行网络探测；MongoDB URI/Database 只检查非空白，驱动语法、认证及拓扑在 P3 验证；模型支持清单与真实窗口在 P6 验证。P1 当时入口为占位；P7 已完成实际加载与启动装配，当前 server 入口通过 joho/godotenv 自动加载工作目录的 `.env`，已有环境变量优先。有界缓冲、租约等选项待对应模块明确消费方与语义后增加，避免提前定义未使用参数。

未解决问题：本项无；真实外部集成仍待后续阶段。下一项为 P1.4 日志。

### P1.4 日志 — 已完成（当前约定：Logrus + 文件轮转）

以下为 P1.4 当时的验收记录；其中暂缓的日志字段脱敏已在 P10.2 补齐，现行契约见 [日志约定](logging.md)。

按用户要求共用 Logrus 默认实例，使用 Lumberjack 文本文件轮转；敏感信息处理暂缓，不作为本次验收项。业务在输出位置通过 WithFields 显式添加 ID，不从 context 隐式提取。

实现文件：

- `internal/observability/logger.go`：Init 配置标准实例及文件输出，Close 关闭当前文件；删除旧 context 关联和安全 writer/formatter。
- `internal/config/logging.go`：Level/Path/Size/Rolls 加载与校验；完整环境变量为 20 项。
- `cmd/server/main.go`：WithFields 输出 process_id，退出时关闭文件；无效配置向 stderr 报告后返回 1。
- `internal/observability/logger_test.go`：实际文件写入、级别过滤、默认实例身份、并发显式 ID 和实际轮转压缩测试。
- `internal/config/logging_test.go`、`config_test.go`、`cmd/server/main_test.go`：迁移已变更接口和配置，验证非法参数及生命周期；移除过时的 safeWriter 测试。
- `.env.example`、日志/配置说明、README、架构及本进度同步更新。

验收已通过：

- 共用默认 logger，无效 Init 不改变现有配置；64 个 goroutine 的五个显式关联字段不串用，后续无字段日志不继承旧 ID。
- 路径非空白且非目录形式；Size 为正且换算字节数无溢出；Rolls 非负，0 表示不限制备份数量；fatal/panic 阈值被拒绝。
- 默认 info、50 MiB、3 个备份；文本日志使用本地时间命名轮转文件并 gzip 压缩。测试验证超过上限后的活动文件与备份内容。
- 骨架启动/退出和配置失败测试通过；无效配置不创建默认路径文件。
- `go test ./...`、`go test -race ./internal/observability ./internal/config ./cmd/server`、`go vet ./...`、`go build ./...` 均通过。

实现边界：Init 仅校验配置，不保证文件可写；首次写入才打开文件。业务停止日志后 Close，不承诺等待后台压缩完成。敏感信息过滤按用户要求暂缓，原生 WithFields/WithError 保持 Logrus 行为；不将其标记为已验证的安全能力。实际业务的关联日志随 P4/P7/P9 实现接入，服务优雅关闭在 P7，指标与追踪在 P10。

未解决问题：本次范围无；敏感信息处理暂缓。下一项为 P1.5 领域状态规则。
### P1.5 领域状态规则 — 已完成

实现文件：

- `internal/domain/status.go`：RunStatus/TaskStatus 的 Validate、IsTerminal、ValidateTransition，TaskStatus.IsPaused；明确未知状态与非法迁移的公共错误类别。
- `internal/domain/task_state.go`：Task.RequestCancel、RecordObservationError、ApplyUpdate；取消意图、观察错误、完整快照更新、终态冻结与可变输入深拷贝。
- `internal/domain/run.go`、`task.go`：补充状态、取消、观察时间、观察错误及完整更新契约的详细中文注释。
- `internal/domain/status_test.go`：迁移表、状态分类、暂停恢复、取消竞争、观察失败、重复更新、终态保护、别名隔离及无副作用拒绝测试。
- README、架构文档与本进度同步更新；核对 go.mod，修正旧文档中仍保留 ADK 依赖的过时描述（当前尚未引入）。

验收已通过：

- 穷举 6×6 Run 和 8×8 Task 迁移组合；零值、空白、大小写变化及未知状态报参数无效，合法状态之间的禁止迁移报冲突，不回显原始状态。
- submitted 可直接到终态；运行与两种暂停可互转，活动任务不能回到 submitted；暂停不触发重执行或消费结果。
- 首次取消仅记录时间，重复取消保留首次时间；取消先到或终态先到均保留实际远端结果。Run 取消后仍可留存 Task 成功，但不能重开 Run。
- 观察失败仅改变安全说明及本地更新时间，成功观察清除错误；不改变远端状态、结果、进度、句柄或最后成功观察时间。
- 同状态进度/游标变化可更新，首次观察和错误恢复会记录；完全相同快照不更新时间。终态完全相同快照幂等，任何不同状态/结果/进度/游标均拒绝覆盖。
- 非终态结果、错误 CallID、未知状态及缺失记录时间被拒绝，失败不产生部分修改；输入进度、结果及嵌套字节切片不会与已保存快照共享。
- `go test ./...`、`go test -race ./internal/domain`、`go vet ./...`、`go build ./...` 均通过。

实现边界：既有 Run.Validate/Task.Validate 保持身份与关联契约，状态另由 Status.Validate 显式校验；Go 字段赋值不会自动强制规则。状态方法操作调用方独占的内存副本，不自增 Version、不设置 AppliedAt、不执行网络调用或聚合 Run 分支。适配器负责保留未知状态诊断、协调活动通知顺序及提供完整快照；终态结果必须在提交前准备好，终态无结果也允许，但不支持提交后补写结果。LastObservedAt 表示最后一次有变化的成功观察，不是轮询心跳。数据库版本比较、检查点与消费原子协调在 P2/P3/P9 实现。

未解决问题：本项无；真实协议映射、远端取消及多实例竞争仍待后续集成验证。下一项为 P2 存储契约补齐。

## P2 存储契约补齐 — 已完成

前置：P1。

实现文件：

- `internal/store/transactions.go`：Run/Task 跨集合事务请求/结果（原接口已在 P3 简化中移除）、Start 输入摘要、创建/提交/跟踪/观察/消费前置校验；移除绕过事务的独立写入接口。
- `internal/store/guards.go`：版本 CAS/溢出、Lease/WriteGuard、Fence/有效期判断、有界分页和 ErrCursorExpired。
- `internal/store/recovery.go`：租约 Acquire/Renew/Release 与独立内部 Recovery.Scan 契约、完整作用域恢复游标校验。
- `internal/store/receipts.go`：MutationReceipt 和作用域/操作种类/内容匹配，明确未知提交结果的幂等重放。
- `internal/store/repositories.go`、`tasks.go`：只读仓储及分页结果、受保护的派生快照保存、检查点/交付读取；ListUnsettled 排除 applied/discarded。
- `internal/domain/checkpoint.go`、`session.go`、`task.go`：Checkpoint、TaskDelivery、ActiveRunID 及 AppliedAt 的详细中文契约。
- `internal/agent/runtime.go`：Request 可携带恢复检查点，说明恢复与消费事务、外部副作用边界。
- `internal/store/contracts_test.go`：租约接管/过期、版本/溢出、幂等摘要、创建关联、分页水位、续接/重复消费、跨分支隔离、回执和迟到任务事件规则的行为测试。
- 新增 [存储契约](storage-contracts.md)，同步架构事务表、索引规划、README 和本进度。

验收已通过（契约及纯逻辑）：

- 会话占用与租约分离，租约过期不启动另一 Run；旧 Owner/Fence/过期凭证拒绝，数据库时钟和同事务写冲突要求明确。
- Start 原子提交输入、Run、会话占用及幂等记录；相同输入重试不受新 ID/预期版本影响，不同输入冲突，摘要保留字段边界及非 UTF-8 字节。
- 消息/事件序号、连续前缀保留水位、排他游标、有界分页及未来/过期游标行为明确。
- 终态与 pending delivery 同事务；检查点、AppliedAt、交付状态、输出及回执同事务消费；已消费/丢弃、跨作用域/分支、错误待完成集合和旧版本被拒绝。
- 终态 Run 可保存远端结果，但不重开事件流；提交回执区分原操作和不同内容，拒绝跨用户读取；恢复扫描不复用空 Scope。
- `go test ./...`、`go test -race ./internal/store ./internal/domain`、`go vet ./...`、`go build ./...` 均通过。

实现边界：没有新增内存假仓储或 MongoDB 适配器；行为测试验证实际校验函数，未把模拟结果当作数据库事务/多实例通过。数据库原子性、回滚、读一致性、回执持久化、租约时钟和故障注入在 P3；运行循环/事件跟随在 P4，SDK 检查点可恢复性在 P6，真实任务续接在 P9。Commit/Apply 的版本化请求摘要编码在 P3 适配器实现。外部模型/工具副作用不承诺恰好一次。

未解决问题：P2 范围内无；P3 需要支持事务的 MongoDB 测试环境，集成验收清单已写入存储契约。下一项为 P3 MongoDB 适配。

## P3 MongoDB 适配 — 已完成

前置：P2。

当前实现约定：按用户要求采用具体方法，不增加 store interface 或泛型 DAO。`Database` 直接持有 MongoDB 实例；`collection()` 只取得原生集合，查询和写入条件在业务方法中可见。保留 schema 编解码、作用域过滤、版本条件和非泛型事务生命周期辅助函数。P2 的历史接口设计已由此替代。

实现文件：

- `internal/adapter/mongodb/database.go`、`documents.go`、`indexes.go`：连接/认证/事务拓扑探测、显式超时/关闭、schema=1 BSON 信封、错误安全包装、snapshot/majority 事务、服务端时钟、作用域唯一/查询索引和非 simple 排序规则拒绝。
- `reads.go`、`snapshots.go`：全部普通仓储、检查点/交付读取、有界一致分页、快照版本和历史覆盖范围校验。
- `runs.go`、`writes.go`：Start/Commit/Cancel 原子提交、输入幂等、版本化提交摘要/回执、消息/事件序号、检查点更新、终态事件与会话释放。
- `leases.go`：数据库 `$$NOW` 条件授予/续期/释放，单调 Fence、实际 Revision 写冲突、旧持有者拒绝及终态维护租约。
- `tasks.go`：Track/Observe/元数据/Apply/Discard 事务，终态与交付原子保存，检查点/AppliedAt/输出/回执原子消费，未结算查询投影。
- `recovery.go`：独立 OpenRecovery、完整作用域管理扫描、连续前缀事件清理与保留水位。
- `codec_test.go`、`integration_test.go`、`fixture_test.go`、平台进程辅助测试文件：编码兼容、安全错误、真实 mongod 临时副本集、故障注入、独立客户端竞争与恢复消费；测试服务仅监听本机并自动清理。
- `direct_methods_test.go`：具体 GetTask 的跨租户/用户隔离及 context 取消；已有检查点被数据库拒绝更新时完整回滚；事务回调重试只推进一次版本，提交失败返回零结果。
- `go.mod`/`go.sum` 锁定官方驱动 v2.9.1；`.gitignore` 忽略 `.local/` 测试二进制。新增 [MongoDB 适配与测试说明](mongodb.md)，同步架构、存储契约、README 和进度。

验收已通过：

- 同 ID 跨用户/租户隔离、空 Scope 拒绝、同会话并发幂等提交、同键不同输入冲突、跨会话同键独立，索引初始化可重复。
- 两个独立客户端 CAS 竞争只有一个成功；租约过期/释放不释放会话，Fence 单调递增，过期/旧持有者拒绝，续期行为正确。
- Start/Commit/Track/Observe/Apply 提交前故障及唯一索引中途失败全部回滚；消息/事件序号与内容共同回滚。
- 服务端 failCommand 注入 UnknownTransactionCommitResult；驱动提交重试及应用回执重放不重复持久化。
- 消息/事件分页、未来/过期游标、全部清理后水位；Run 终态事件、会话释放，终态后任务观察不重新追加事件。
- 终态首次登记、重复快照、观察错误、取消意图、跨分支拒绝、重新连接后消费、保留其他待完成调用、Apply/Cancel 跨客户端竞争、discarded 排除和缺失交付诊断。
- 快照 CAS/覆盖范围、恢复跨租户分页及重扫；真实 standalone 和不安全集合排序规则被拒绝。编码保留 nil/空切片/原字节，摘要固定兼容样本及错误原因链测试通过。
- 全量 test、mongodb/store/domain race、vet、build 均通过；真实集成环境及复现命令见 MongoDB 说明。

实现边界：MongoDB Community 8.0.32 单节点副本集验证了真实事务和独立客户端竞争；没有声称验证多节点选举/网络分区、mongos、认证/TLS 或生产容量。恢复测试是数据库重连和持久检查点/交付消费，ADK 真实恢复在 P6/P9。P3 当时不新增 HTTP/模型/工具执行；P7 已完成数据库与 HTTP 启动装配。租期至少 1 ms（MongoDB 日期精度）；普通 Open 与管理 OpenRecovery 分离，实际部署 RBAC 在装配时配置。

未解决问题：P3 范围内无；部署级故障/容量验证在 P10 扩展。下一项为 P4 应用服务与事件流。

## P4 应用服务与事件流 — 已完成

前置已核对：P3 具体数据库方法、事务、租约、回执与分页均已实现；本阶段运行时使用接口替身，真实 ADK 在 P6。

实现文件：

- `internal/app/application.go`：会话与 Run 用例；请求独立生命周期、租约领取/续期、取消、关闭、恢复接入、上下文准备扩展点及中文并发/故障语义注释。
- `internal/app/service.go`、`events.go`：应用契约注释；带租约/版本/稳定 OperationID 的事务发布；一致快照分页轮询、同步背压、终态退出和游标错误。
- `internal/agent/runtime.go`：Update 可携带完整 assistant 消息，与事件同事务持久化；明确输出种类与 Emit 生命周期。
- `internal/store/repositories.go`、`internal/adapter/mongodb/reads.go`：EventPage 增加同快照 Terminal，解决终止游标重连与终态检查竞态。
- `internal/app/application_test.go`、`fixture_test.go`、`process_*_test.go`：运行时替身、真实临时副本集、独立数据库客户端行为测试；Windows 测试数据库隐藏窗口。
- `docs/application.md`：装配、生命周期、恢复、背压、部署及验证说明；同步 README、架构与进度。

验收已通过：

- 会话创建/查询、Run 创建/查询/取消；输入和占用原子提交。同键并发返回同 Run 且只执行一次；不同输入/活动会话冲突，跨用户访问与取消拒绝。
- 请求结束不取消运行；输入只在历史出现一次，完整输出消息落库。运行成功/错误分别提交 completed/failed，终态释放会话；非法终态输出被拒绝且不能伪报成功。
- 租约续期跨越多个租期后仍可写入；跨客户端取消撤销租约，运行 context 停止，迟到输出拒绝；重复取消保留实际终态。
- 关闭取消执行并等待，关闭后拒绝新 Run。queued 恢复走首次执行，已开始 Run 未配置恢复回调则明确拒绝，不重新 Execute；配置回调后通过专门恢复路径处理。
- 发布失败无可见事件，回执重放不重复分配序号。跨客户端在追赶期间/空闲跟随期间提交均连续可见；慢回调不阻塞写入，缓存限于一页。
- 终态流退出、已消费终止游标立即结束；消费者错误、订阅取消、未来游标、清理后过期游标及无效选项行为正确。
- 全量 test、app/mongodb/store/domain race、vet、build 均通过；真实 MongoDB 8.0.32 临时副本集测试未跳过，命令见本文件顶部与应用说明。

实现边界：事件传播选定数据库有界分页轮询，无 change stream/本地通知依赖。慢消费者采用同步背压，HTTP 回调的写超时/断连控制在 P7；单条事件大小和全局容量限制在 P7/P10。默认读取原始历史，不做 P5 Token 预算；不声称已接入真实模型或 SDK 恢复。P4 提供 Prepare/RecoverRun/Recover 接入点，P6/P9 接入真实检查点、任务等待聚合和恢复扫描；没有工具执行队列。服务关闭或写入结果不确定时保留持久 Run，禁止盲目重执行。运行时必须合作响应 context，无法强制停止任意第三方代码。

未解决问题：P4 范围内无。真实 HTTP/SSE、ADK、任务续接与多节点部署验证按后续阶段执行。下一项为 P5 上下文基础能力。

## P5 上下文基础能力 — 已完成

前置已核对：P4 已提供 Options.Prepare，数据库可按作用域分页读取完整历史与最新摘要；准备失败不会调用 Runtime。

实现文件：

- `internal/contextengine/engine.go`、`prepare.go`：补齐 Input.RunID/PolicyVersion 契约、Budget.InputLimit、Builder、工具配对检查、摘要与原文选择、整体计数、明确超预算错误及深拷贝，附详细中文注释。
- `internal/domain/session.go`：新增 PartText/PartToolCall/PartToolResult 常量及模型历史调用 ID 约定，不改变领域字段、BSON 布局或已有输入摘要编码。
- `internal/app/context.go`：NewContextPreparer 按页读取历史与最新摘要，为 P4 Options.Prepare 提供可直接注入的预算化实现；固定系统约束在构造时隔离复制。
- `internal/app/application.go`、`internal/config/config.go`：同步准备接入和策略版本的中文契约说明，无新增环境变量。
- `internal/contextengine/prepare_test.go`：8 组行为测试，覆盖预算、组装、工具、摘要、错误、取消、并发与别名隔离。
- `internal/app/context_test.go`：2 组真实副本集接入测试，覆盖跨页历史/最新摘要、当前输入唯一、只读准备和超预算阻止模型调用。
- `docs/context.md`：完整输入、工具历史、角色信任、预算、装配与验证契约；同步 README、架构、应用及配置说明、本进度。

验收已通过：

- 固定系统约束在最前，摘要作为低信任派生资料，之后按序保留未覆盖历史和当前运行消息；已持久化当前输入不重复追加。
- 输入预算逐项扣减防溢出，完整最终消息只计数一次；等于限额可通过，超一 Token/超长单轮明确报 ErrBudgetExceeded（同时属于 ErrInvalidArgument），失败无部分 Prepared。
- 不使用快照 TokenEstimate 代替模型计数；计数器错误、取消及超时保持可识别，零/负计数拒绝。
- 摘要不替代任何工具调用/结果或待完成调用，保留承载消息完整内容；拒绝孤立/重复/乱序结果、角色/名称错配和含糊工具关联，跨 Run 同 ID 独立配对。
- 拒绝跨作用域/会话、非连续/重复历史、伪装系统历史、越界/未持久化/策略不匹配摘要；工具原文不提升为系统指令。
- 输入、计数器副本和输出的消息/字节/快照互不共享可变数据；并发不同用户不串用上下文。
- 真实 MongoDB 历史跨越 128 条分页，使用最新摘要且当前输入只有一份；准备不改写原始历史/摘要；超预算在 Runtime 调用前使 Run 失败，无摘要正常路径通过。
- 设置 CAGENT_TEST_MONGOD 后全量 test、contextengine/app/mongodb/domain race、vet、build 均通过；MongoDB 8.0.32 临时副本集集成测试未跳过。

实现边界：本阶段选择计划允许的“明确超预算”策略，不静默裁剪用户要求、不截断工具对、不生成摘要。P10 实现有界压缩及关键要求保留。计数器可替换，测试使用可控替身；P6 必须验证真实模型 TokenCounter、窗口与 SDK 消息映射。P4 原始历史默认路径保留用于替身，P6 装配真实模型必须注入 NewContextPreparer 或等价的预算化准备流程，模型工具循环的每次新输入亦需校验。Task 的原始分支路由不变；模型历史 ToolCallID 在同 Run 唯一，提供方局部 ID 映射在后续适配器完成。

未解决问题：P5 范围内无。真实模型计数、ADK 与检查点恢复按 P6/P9 验证。下一项为 P6 ADK 最小执行链路。

## P6 ADK 最小执行链路 — 代码与本地验收完成，真实模型验收待配置

前置：P4、P5。

- 引入并锁定 ADK 版本，核对实际 API 和恢复能力，确认检查点方案；不能用模拟恢复替代真实能力。
- 实现 `internal/adapter/adk` 的 Runtime.Execute，配置 Agent、模型及会话桥接。
- 转换消息和流式输出，关联 Agent/Subagent 调用身份，持久化最终消息及运行状态。
- 接入检查点边界和取消，保持并发用户状态隔离。
- 验收：接口替身测试和配置了凭据的真实模型冒烟验证；输入到持久化输出贯通，异常和取消可追踪。
- Runtime.Resume 的完整任务续接行为在 P9 完成。

实现文件与行为：

- `internal/adapter/adk/{runtime,messages,openai,checkpoint}.go`：锁定 ADK v2.4.0、OpenAI Go v3.66.0，真实 Runner/LLMAgent、独立会话、结构化消息映射、Chat Completions SSE、同映射 BPE 估算及执行前预算复核、版本化完成检查点与恢复；补充中文职责及故障语义注释。
- `internal/config/{config,load,model}.go`、`.env.example`：动态模型名、API 前缀、API 密钥、显式编码、输出限额字段和请求超时；用户指定 OpenAI 兼容方式，不接入 Gemini 默认模型。
- `internal/app/adk.go`、`application.go`、`internal/agent/runtime.go`、`internal/domain/run.go`：P4/P5 工厂装配，最终消息/事件/检查点同事务提交；`message.completed` 不重复作为增量。已提交输出后重启只结算 Run。
- ADK、OpenAI、配置及应用行为测试：真实 SDK 暂停/序列化/新 Runner 恢复探针，本地 HTTP 协议与真实 MongoDB 集成；覆盖错误、取消、超时、并发隔离、预算、整数精度和不重复执行。
- [ADK 接入说明](adk.md)、配置、架构、README 与进度同步更新。

本地已验证：输入经过预算与真实 ADK 到 HTTP 协议服务，再持久化消息/事件/检查点和 Run 终态；幂等重放不重调模型；服务在输出提交后退出可恢复结算；截断流不会写完整消息或完成检查点。真实 SDK 长工具暂停/恢复使用实际会话事件与状态，模型为替身；不把该探针视为生产 P9 已实现。

未解决验收：当前没有真实提供方配置/凭据，`TestRealOpenAISmoke` 默认跳过。需环境配置后显式 `CAGENT_TEST_MODEL=1` 运行，检查提供方协议与服务端 usage。BPE 是显式编码下的估算，不保证任意兼容模型的精确计数或窗口；窗口与安全余量由部署配置。启动入口、HTTP、工具执行、子 Agent 调度及 TaskDelivery 续接仍分别属于 P7/P8/P9。

## P7 HTTP 与启动装配 — 已完成

前置核对：P4/P5/P6 本地实现可复用；P6 真实提供方凭据未配置，仍保留外部模型验收待办，不以本阶段本地替身结果替代。

实现文件：

- `internal/transport/httpapi/api.go`：Gin 路由、固定 HS256 JWT 验证、可信 Scope、显式 DTO、必要 JSON 解码、请求体限制、安全错误、服务端请求 ID、panic 恢复、SSE 编码及有界订阅。
- `internal/app/events.go`：增加 CheckCursor，在 SSE 提交响应前校验作用域和持久水位，Follow 继续每页校验。
- `internal/bootstrap/server.go`、`cmd/server/main.go`：完整配置、日志、数据库/索引、预算化 ADK 应用与 HTTP 装配；健康/就绪、启动失败清理、SIGINT/SIGTERM 与优雅关闭。
- `internal/adapter/mongodb/database.go`：增加无资源读取的 Ping 就绪探针。
- `internal/config/http.go`、`config.go`、`load.go`：JWT、单次写超时、请求体上限；HTTP.ValidateServer 不强迫离线组件配置认证。
- `internal/transport/httpapi/*_test.go`、`internal/bootstrap/server_test.go`、`internal/config/http_test.go`：JWT/HTTP/SSE、真实 TCP 背压、真实副本集端到端与启动关闭行为测试；既有配置/入口测试同步迁移。
- Gin v1.11.0、JWT v5.3.1；保留 ADK v2.4.0/OpenAI v3.66.0。`.env.example`、README、架构、配置和新增 `docs/http.md` 同步更新。

验收已通过：

- 验证签名、固定算法、签发方、受众、必需 exp、可选 nbf 和身份字段；缺失/错误/过期令牌拒绝，普通头与 JSON 无法覆盖身份。
- 会话创建/查询，文本 Run 提交（202）/查询/取消（204），服务端 Agent 选择；同键重试不重复生成，输入差异/活动会话冲突返回 409。
- 真实 MongoDB 下跨用户/租户的会话、Run、提交、取消、SSE 访问均返回 404。
- Last-Event-ID 排他重放、心跳注释、终态自动关闭、未来/无效游标 400、清理后旧游标 410；真实 TCP 慢客户端被写超时断开，订阅不会无限预取。
- HTTP 断连不取消 Run；真实 JWT/Gin → ADK/OpenAI SDK → MongoDB → SSE 链路在断连后仍持久化完整输出；模型提供方为本地可控 HTTP。
- 生产装配入口可监听并通过就绪探针，关闭后释放端口；占用端口和缺失认证配置明确失败。持续请求在应用关闭前结束，不合作 handler 受宽限期限制。
- 设置 `CAGENT_TEST_MONGOD` 后 `go test ./... -count=1` 通过，数据库集成未跳过；`go test -race ./internal/transport/httpapi ./internal/bootstrap ./internal/config ./cmd/server ./internal/app -count=1`、`go vet ./...`、`go build ./...` 通过（2026-09-24，Windows/amd64，Go 1.27.1，MongoDB 8.0.32 单节点副本集）。

实现边界：按用户要求只做必要 HTTP 解码/大小/游标限制，不完善所有参数校验。JWT 由可信身份服务签发，本服务不实现登录/签发；健康探针不需认证，就绪只验证数据库。SSE data 是 JSON 信封，内部原始数据以 base64 保真；开始后的错误结束连接，客户端须重连。Task 查询/取消及跨租户恢复扫描属于 P9，本阶段不注册伪成功接口或启动 Worker。真实提供方、生产 TLS/认证拓扑、多节点故障验证仍待外部环境。

P7 后续交接：P8 已完成下述代码与本地验收；环境就绪后补 P6 真实模型冒烟。

## P8 工具注册与协议适配 — 代码与本地验收完成，外部验收待配置

前置：P7。

### P8.1 工具注册与本地工具 — 已实现

- `internal/tool/catalog.go`：不可变 Scope 注册快照；发现/解析/执行重验权限，JSON Schema、参数大小、超时、实际 ToolOutcome 校验，业务错误结果、artifact 引用策略、任务客户端包装。
- `internal/bootstrap/tools.go`、`internal/config`：`CAGENT_TOOLS_FILE` 可信 JSON 配置；每连接凭据环境引用白名单、工具白名单、启动发现与关闭。内置 echo，业务工具可通过 ExecutorFunc 注册。
- `internal/adapter/adk/tools.go/runtime.go/openai.go`：工具声明及实际请求预算；有界多轮调用；兼容函数名别名；即时调用/结果回传与历史配对；任务动态暂停和 `cagent.adk.tools.v1` SDK 快照。
- `internal/agent/runtime.go`、`internal/app/application.go/adk.go`：Update.MessageRole/Tasks、ErrWaiting；当前 Fence 下调用 P3 TrackTask 事务交接任务/检查点/事件，保留 waiting_tool 和会话占用。
- `catalog_test.go`、`adk/tools_test.go`、`app/tools_test.go`：隔离、未知工具、Schema、无效返回、业务错误、artifact、并发、超时；真实 SDK 即时/任务行为；真实副本集 + OpenAI HTTP SDK + ADK 的消息对、双任务及检查点交接。

### P8.2 MCP 适配 — 已迁移官方 SDK（MCP tasks 暂不支持）

- `internal/adapter/toolhttp`：只保留 Scope 校验、凭据按请求解析、禁止重定向/跨源、HTTP 超时/总响应上限；已移除自建 JSON-RPC/SSE 客户端。
- `internal/adapter/mcp`：使用 `modelcontextprotocol/go-sdk v1.8.0` 的 Client/ClientSession/StreamableClientTransport；固定 MCP 2025-11-25，发现/分页、普通工具调用及领域结果转换由适配层衔接。
- SDK 暂无 tasks API，MCP Get/Follow/Cancel 明确返回 Unsupported，不重执行旧句柄；不发送 task 参数，不声称支持 MCP 长任务。SDK 不保留 execution.taskSupport，要求任务模式的工具由提供方拒绝普通调用。关闭交给 SDK。
- 真实 SDK 服务端互通测试覆盖发现、结果类型、凭据/作用域和会话关闭；本地协议夹具验证 task-only 响应拒绝及不重试。HTTP 策略测试覆盖超时、断连、超限、重定向、跨源和凭据轮转。

### P8.3 A2A 客户端适配 — 已迁移官方 SDK

- `internal/adapter/a2a`：使用 `a2aproject/a2a-go v0.3.15` 的 agentcard.Resolver/a2aclient.Client，保留 A2A 0.3.0 JSON-RPC、Message/Task、Get/Cancel/Resubscribe、状态映射与暂停输入/认证。只允许已验证的配置端点。
- `tool.TaskClient` 仍接收原 ToolCall，终态结果关联 CallID；TaskInteractor 仅补充原 taskId/contextId，JWT 路由和持久化交接不变。
- SDK 不暴露 SSE 事件游标，非空旧 Cursor 返回 Unsupported；无游标订阅仍用通知触发完整查询，流提前结束返回观察错误，不视为任务终态。
- 本地 HTTP/SSE 提供方验证所有任务状态、未知状态、取消竞争、暂停输入/凭据、任务关联及旧游标拒绝。外部提供方仍待配置验收。
中文注释、README、架构、配置/HTTP/ADK 文档及 [工具说明](tools.md)、[配置示例](tools.example.json) 已同步。

验证命令见本文件顶部。MongoDB 8.0.32 真实临时副本集已参与全量与竞态测试，工具提供方使用真实 HTTP/SSE 的本地协议夹具。`TestExternalToolSmoke` 提供显式开关及指定调用输入，本次环境未配置，默认跳过；外部模型及真实 MCP/A2A 提供方兼容验收仍待完成。

P9 交接：本阶段只登记并暂停远端任务，不启动持续跟踪或自动续接。P9 需接 TaskClient 的观察/退避、终态 TaskDelivery 消费、Runtime.Resume、查询取消 API 和恢复扫描。多任务逐个 TrackTask 原子登记，SDK 检查点 Data 保留整批句柄，部分登记后崩溃应补齐缺失 Task，不重执行。外部启动成功但尚未落任何句柄的窗口仍不保证恰好一次。默认 artifact 策略为拒绝超限；可注入持久 ArtifactWriter，未内置 artifact 存储服务。

下一项：P9 长任务与恢复；外部模型/工具冒烟按环境配置补验。

## P9 长任务与恢复 — 代码与本地验收完成

前置已核对：P8 的句柄/SDK 暂停交接与协议客户端已存在；P3 的租约、终态交付、Apply/Discard 事务可复用。原先只有这些边界，未实现观察、生产 Resume 或恢复扫描。本次完整接通，未增加独立 Worker/执行队列。

实现文件：

- `internal/app/tasks.go`、`task_tracker.go`：绑定租约/检查点的 Track/Get/Follow/Cancel；有界并发订阅/查询、游标、错误记录与退避、终态交付和同步 ApplyTask 接纳。
- `internal/app/recovery.go`、`branches.go`、`application.go`、`adk.go`：服务级扫描/关闭、同名 Agent 不同 Invocation 精确调度、全分支等待聚合、取消后的维护、恢复前 in-flight 屏障及工具注册/任务配置自动装配。
- `internal/adapter/adk/resume.go`、`checkpoint.go`、`runtime.go`、`tools.go`、`messages.go`：真实 SDK 结果接纳/新 Runner 推进、完整历史与模型轮数保存、P8 快照兼容、整批句柄补齐、重复结果/调用保护，以及外部启动结果不确定的错误语义。修复 JSON 解码复用对象残留旧 FunctionResponse 的问题。
- `internal/adapter/mongodb/task_cancel.go`、`checkpoint_reads.go`、`internal/store/transactions.go`、`tasks.go`：用户取消意图事务、作用域内分支分页、已终态 Run 从已有检查点补齐句柄（不追加终止后的事件）。
- `internal/bootstrap/server.go`：独立管理连接和恢复扫描的启动/反向关闭；任务参数使用现有环境配置。
- `internal/transport/httpapi/api.go`、`task_dto.go`：JWT 保护的任务查询/取消、显式 DTO，取消 202 仅表示意图已提交。
- `internal/agent/runtime.go`、`tasks.go`：明确接纳输出、运行跟踪接口及不确定检查点契约，新增/复杂逻辑附中文注释。
- 新增 `internal/app/tasks_test.go`、`branches_test.go`、`task_crash_test.go`、`task_observation_test.go`、`internal/adapter/adk/resume_test.go`、`internal/transport/httpapi/tasks_test.go`；同步 README、架构、HTTP、配置、应用、ADK、工具/存储说明及 [长任务说明](tasks.md)。

本地验收已通过：

- 真实 ADK/OpenAI SDK、本地 HTTP 模型、MongoDB：双任务只执行一次，乱序结果各自接纳，结果消息与检查点/AppliedAt/delivery 原子保存；消费回调失败前后均不提前调用模型，重复接纳拒绝。
- 同名 Agent 的不同 Invocation 独立续接；已就绪分支不等待另一分支结束，其他分支等待时 Run 不提交 completed。应用分支调度用运行时替身验证，SDK 恢复使用真实 Runner 单独验证。
- 订阅优先、持久游标、通知触发完整查询、能力不足/断流/超时回退；观察失败只记录 ObservationError，不转成远端 failed，不重新 Execute。
- 首个 Track 后中断时从 SDK 整批句柄补齐剩余 Task；即使 Run 已取消仍补齐、跟踪实际结果并 discarded，终止事件后没有新 SSE。
- 两个独立数据库客户端/应用/扫描器竞争同一 Run，通过租约和版本保护恢复；任务句柄保持原 ID，结果只消费一次。
- 任务取消先持久意图，不支持取消时保留实际成功；Run 取消后不继续生成，本机维护和重启后的维护均可保存迟到结果。
- input_required/auth_required 通过原 API 继续同一远端任务，之后观察和模型续接自动完成；HTTP Scope/私有字段隔离、持久 Follow 断连和 SSE 连续序号/唯一终止水位均验证。
- 结果接纳后再次生成期间中断：持久 `cagent.in-flight/v1` 屏障阻止从旧检查点重放，恢复以 `execution_outcome_uncertain` 记录失败原因。无法取得句柄的工具错误同样禁止盲目重试。
- 全量 test、关键包 race、vet、build 通过；真实副本集集成未跳过。race 首次发现新增测试的非原子并行计数，改用原子计数后通过。

实现边界：外部模型/工具副作用不保证恰好一次。当前提供方接口没有通用按调用查询能力，未持久句柄的窗口使用明确不确定结算，不能声称已自动找回远端任务。官方 MCP SDK 不支持 tasks API；A2A SDK 不暴露 SSE ID，恢复依靠完整查询。外部提供方、多节点选举/网络分区、生产认证/TLS 仍待配置验证，不标记为通过。全局容量、压缩、指标及完整部署验收属于 P10。

下一项：P10 上下文压缩与运行保障；按环境配置补验 P6/P8/P9 外部提供方。

## P10 上下文压缩与运行保障 — P10.1/P10.2 已完成

前置：P9。

### P10.1 压缩策略 — 已完成

- 实现阈值、近期轮次保留、摘要模型与策略版本配置。
- 保存 ThroughSequence 和并发版本，拒绝旧摘要覆盖新历史。
- 保留系统约束、关键用户要求、完整调用对和未完成调用；原始历史始终保留。
- 压缩失败或仍超预算时有界回退/明确报错，不直接发送超长输入。
- 验收：并发压缩、摘要失败、超预算、调用配对和待完成任务上下文正确。

实现文件：

- `internal/contextengine/compression.go`：无会话共享状态的压缩策略；按输入预算百分比触发，近期轮次包含当前轮；完整校验后至多调用一次 Summarizer，重新整体计数后才返回候选。移除未使用的 Compressor 占位。
- `internal/contextengine/engine.go`、`prepare.go`、`internal/domain/context.go`：待保存候选及中文契约；压缩路径保留全部用户原文、系统约束、完整工具消息及当前 Run，原历史不删除。
- `internal/adapter/adk/summary.go`：独立摘要模型/编码/窗口/输出预算，无工具、非流式、零自动重试；摘要输入超限在 HTTP 前拒绝，空结果/截断/错误不伪造成功。
- `internal/config/compression.go`、`config.go`、`load.go`、`validation.go`、`.env.example`：六项新环境配置，保留已有 PolicyVersion；默认 80%、近期 2 轮，摘要模型/编码为空时沿用主模型，窗口 8192、输出 512。
- `internal/app/context.go`、`adk.go`、`application.go`、`internal/agent/runtime.go`：生产装配；准备携带候选与读取时会话版本，持有者在写锁内复用 SaveSnapshot，成功后刷新 Run.Version 再调用运行时；生成摘要期间允许租约续期。
- `compression*_test.go`（contextengine/config/app）、`summary_test.go`、`snapshot_concurrency_test.go`：阈值边界、近期保留、用户要求、工具对/待完成调用、策略迁移、并发隔离、故障回退、预算/取消、真实 HTTP 模型参数、独立 MongoDB 客户端竞争和过时历史拒绝、实际 ADK 端到端验证。原配置测试同步更新。
- `docs/context.md`、`docs/configuration.md`、`docs/architecture.md`、README 与本文件同步更新。

验收已通过：

- 阈值达到即触发；无可压缩前缀不调用摘要，同策略同水位不重复摘要。策略升级从完整原始前缀重建；扩大近期范围时不使用覆盖过多历史的旧摘要。
- 摘要可能遗漏信息，因此用户原文全部保留，不依赖模型判断关键要求；工具调用/结果承载消息、未完成调用和当前 Run 不被压缩。
- 每次准备最多一次摘要请求，无无限重试或截断；摘要失败、空白或无缩减时仅回退到已通过预算的上下文。没有安全回退则明确失败，取消始终传播。
- 摘要输入独立计数，最终主模型输入重新计数；摘要请求中的历史作为不可信 user JSON，不携带可执行工具定义或孤立协议调用。
- MongoDB 两个独立连接竞争同一候选基线只有一个成功；Run/会话/摘要版本与 Fence 拒绝过时写入，原历史条数不变。
- 真实临时副本集 + 本地 OpenAI HTTP + 真实 ADK 连续两次生成/保存摘要并完成 Run，版本、水位推进正确；主模型请求前快照已可读；取消后迟到摘要不保存、不调用运行时。
- `go test ./... -count=1 -timeout=180s`、`go vet ./...`、`go build ./...` 通过；`go test -race ./internal/contextengine ./internal/config ./internal/adapter/adk ./internal/adapter/mongodb ./internal/app -count=1 -timeout=180s` 通过。

实现边界：压缩在新 Run 的上下文准备阶段执行；已有 SDK 检查点按原分支恢复，不改写待完成任务检查点。运行中的工具循环仍逐次检查预算，超限明确失败。摘要前缀过大、受保护消息自身超过主模型窗口时不会无限分块或删要求；有安全原输入则回退，否则报错。摘要文本不承诺语义无损，但用户要求和工具关联通过原文保留。真实摘要提供方的语义质量/usage 校准尚待环境配置；容量、指标、追踪及整体交付留给 P10.2/P10.3。

未解决问题：本项代码与本地验收无；外部模型验收仍待配置。下一项为 P10.2。

### P10.2 容量、指标和追踪 — 已完成

- 限制运行、模型调用、任务观察、工具输出及 SSE 订阅资源，明确过载响应和背压。
- 接入请求、模型、工具、存储、SSE 和任务恢复指标及追踪，控制标签基数和敏感字段。
- 明确事件/任务/检查点保留策略、TTL、artifact 存储与清理责任，保证恢复窗口一致。
- 验收已通过：慢消费者、长时间任务、过载、断连重连、日志脱敏和资源释放。

实现文件：

- `internal/config/capacity.go`、config/load/http/validation、`.env.example`：四个容量参数，默认值、环境加载、边界与启动校验。
- `internal/observability/runtime.go`、`redaction.go`、`logger.go`：无等待队列的容量闸门、幂等释放、固定 operation 指标、256 条有界父子追踪、字段白名单及错误类别脱敏；沿用 Logrus 默认实例和文件轮转。
- `internal/app/application.go`、`adk.go`、`tasks.go`、`recovery.go`：落库前预留、恢复满载跳过、共享模型/观察容量、关闭归还、后台追踪关联与模型过载失败原因。
- `internal/adapter/mongodb/runs.go`、`telemetry.go`、`database.go`：满载时只读幂等重放及输入指纹核对；MongoDB 命令完成/失败监控，不记录命令或回复正文。
- `internal/adapter/adk/openai.go`、`internal/tool/catalog.go`、`internal/transport/httpapi/api.go`：模型/工具/请求/SSE 观测，模型流完整生命周期限制，SSE 503 与 Retry-After，独立凭据保护的 `/debug/metrics` 和 `/debug/traces`。
- 各层新增 `capacity_test.go`、observability/runtime_test、mongodb/telemetry_test；迁移默认配置契约和观察器测试装配。详细语义与保留表见 [运行保障说明](operations.md)，架构、配置、日志、HTTP、README 同步更新。

验收已通过：

- 真实 MongoDB 验证新 Run 满载拒绝不写会话/输入/Run；同键同输入仍能找回原 Run，不同输入冲突；取消与关闭释放槽位；恢复满载不修改 queued 候选，容量恢复后可完成。
- 主模型与摘要模型共享闸门，过载无网络请求；消费者提前停止释放模型流；模型容量失败有持久 `overloaded` 原因且不自动重试工具。
- SSE 满载返回 503/Retry-After；断连后 Follow 退出，重连可重新领取；原慢 TCP 消费者、游标重放及任务长时间观察/恢复测试继续通过。
- 固定类别指标不会用用户/工具/URL 作标签；环形追踪有界且返回副本，父子关联不把 HTTP 取消传播给 Run；MongoDB 同请求号不同连接正确配对；日志未知字段和原始错误不输出。
- 设置 `CAGENT_TEST_MONGOD=.local/mongodb/mongodb-win32-x86_64-windows-8.0.32/bin/mongod.exe`，`go test ./... -count=1 -timeout=180s` 通过，使用真实临时单节点副本集，无数据库集成跳过。
- `go test -race ./internal/observability ./internal/config ./internal/app ./internal/adapter/adk ./internal/adapter/mongodb ./internal/tool ./internal/transport/httpapi -count=1 -timeout=240s`、`go vet ./...`、`go build ./...` 通过。

实现边界：容量按实例计算，不提供集群配额或租户公平调度。指标为进程累计，追踪为本地有界诊断，不包含 OTLP 导出或跨重启持久追踪。默认无业务 TTL、无自动破坏性清理；活动任务/待交付/检查点/回执保留，事件只能通过原子前缀清理管理方法处理。artifact 未装配时拒绝超大结果，自定义后端负责持久性、授权与引用生命周期一致的清理。日志 Message 必须是代码中的固定文本，字段过滤不识别任意自然语言秘密。

未解决问题：P10.2 本地实现与行为验收无；生产负载目标、外部模型/工具、多节点故障与真实 artifact 后端仍待环境配置，不标记为外部验收通过。下一项 P10.3。

### P10.3 整体验收与交付 — 代码与本地验收完成，外部验收待验证

- 建立端到端场景：认证→会话→Run→模型→工具→任务→续接→SSE 重放→重启恢复。
- 运行相关单元/集成测试、全量测试、vet、build；在支持的工具链环境执行关键并发 race 检查。
- 执行多实例隔离、事务/连接故障、取消竞争和恢复故障注入；记录环境及可重复步骤。
- 完成 API、配置、部署、MongoDB 拓扑/索引、模型与工具凭据配置、排障及限制说明。
- 全部范围内功能及真实外部集成通过后才标记项目完成；缺少凭据或测试环境的项目保持待验证。

实现文件：

- `internal/transport/httpapi/acceptance_test.go`：详细中文契约注释；真实 JWT/Gin、OpenAI SDK/ADK、A2A SDK、本地 HTTP 提供方与 MongoDB 组成同一链路，覆盖任务观察 503、应用重建、独立数据库连接、启动扫描续接、幂等重放、跨租户/用户隔离和逐条 SSE 序号比对。
- `internal/adapter/mongodb/connection_failure_test.go`：临时 MongoDB 的 commitTransaction failpoint 真实断开 TCP，断言注入次数、重连提交、回执一致、Run 版本只增一次和事件不重不漏。
- `scripts/acceptance.ps1`：强制临时真实 MongoDB，完整 test/race/vet/build；拒绝非预期测试跳过，保存 JSON 证据，外部冒烟未配置时明确待验证。纯接口包无测试文件不算行为测试跳过。
- `docs/acceptance.md`：场景/专项测试映射、环境和复现命令、外部验收门槛；`docs/deployment.md`：构建、拓扑与索引、凭据与授权、SSE 代理、多实例、关闭升级、保留责任和排障。README 与架构同步。

验证已通过（2026-09-28）：

- Windows amd64、Go 1.27.1、MongoDB 8.0.32 临时真实单节点副本集；运行 `./scripts/acceptance.ps1` 成功，完整命令见 `docs/acceptance.md`。
- `go test ./... -json -count=1 -timeout=240s`：全量通过，无数据库集成跳过；仅 `TestRealOpenAISmoke` 和 `TestExternalToolSmoke` 因无外部环境跳过。
- `go test -race` 检查 observability/config/app/ADK/MongoDB/A2A/MCP/tool/HTTP/bootstrap 共 10 个关键包通过；`go vet ./...`、`go build ./...` 通过。
- 新增整体测试确认原 A2A 工具执行一次、模型两次、结果 applied、重启后幂等与作用域隔离、SSE 与持久事件逐条一致；新增 TCP 故障确认实际触发并重连成功，无重复事件或版本增加。
- 既有双实例恢复、取消竞争、事务回滚、未知提交、部分交接与不确定恢复屏障测试随全量及 race 一起通过。JSON 测试证据存放 `.local/acceptance/tests.jsonl`。

实现边界：新端到端测试通过关闭旧应用后重建应用和客户端模拟重启，不等同 OS 强杀；真实 TCP 提交断连与既有事务回滚/未知提交、取消竞争、双实例恢复、部分交接故障测试共同构成本地故障验收。未执行生产多节点选举、网络分区、mongos、认证/TLS、真实外部模型/摘要/MCP/A2A 生命周期和 artifact 后端验收，不把受控提供方等同外部通过。

下一项：配置实际部署环境与提供方，依 `docs/acceptance.md` 完成外部验收并补充发布记录；全部范围通过前保持项目未全部完成。

## 待实现时确定的选择

以下是未来模块的设计输入，不影响已完成的 P1；不得默认为已确定或已支持：

- 认证提供方与验证方式、固定租户配置及工具授权策略。
- 模型提供方、真实凭据和 ADK 检查点/恢复 API 的兼容性。
- MongoDB 生产部署拓扑与事件保留期限；P4 已选定分页轮询传播。
- MCP/A2A 提供方、所支持的任务/订阅/取消/补充输入能力。
- 大结果 artifact 后端、容量目标及运行时限。

## 交接记录

| 日期 | 完成内容 | 验证 | 下一项 |
| --- | --- | --- | --- |
| 2026-09-23 | P1.1 领域身份与关联校验，详细中文注释及文档 | go test ./...、go vet ./...、go build ./... 均通过 | P1.2 公共错误约定 |
| 2026-09-23 | 建立本实施计划与跨会话进度记录 | 核对当前文件与接口；仅文档变更 | P1.2 公共错误约定 |
| 2026-09-23 | P1.2 公共错误包、领域迁移、存储兼容别名、中文注释与行为测试 | go test ./...、go vet ./...、go build ./... 均通过 | P1.3 配置 |
| 2026-09-23 | P1.3 默认配置、环境加载、集中校验、中文注释、行为测试及配置示例 | go test ./internal/config、go test ./...、go vet ./...、go build ./... 均通过 | P1.4 日志 |
| 2026-09-23 | P1.4 按用户选型使用 Logrus，日志配置、关联隔离、安全错误及骨架生命周期 | 全量 test/vet/build、observability/config/server 的 race 测试均通过 | P1.5 领域状态规则 |
| 2026-09-23 | 按用户要求调整 P1.4：共用 Logrus 默认实例，移除 Logger/New，业务使用原生包级 API，安全策略迁至 formatter | 全量 test/vet/build、observability/config/server 的 race 测试均通过 | P1.5 领域状态规则 |
| 2026-09-23 | 按用户新约定修订 P1.4：Lumberjack 文件轮转、WithFields 显式 ID、参数校验；迁移测试，敏感处理暂缓 | 全量 test/vet/build、日志及配置/入口 race 均通过 | P1.5 领域状态规则 |
| 2026-09-23 | P1.5 Run/Task 状态规则、取消与观察语义、终态快照保护、中文注释及行为测试；P1 全部完成 | 全量 test/vet/build、domain race 均通过 | P2 存储契约补齐 |
| 2026-09-23 | P2 原子存储契约、租约 Fence、幂等/回执、序号分页、检查点及交付恢复记录、中文注释与行为测试 | 全量 test/vet/build、store/domain race 均通过；真实数据库集成待 P3 | P3 MongoDB 适配 |
| 2026-09-23 | P3 MongoDB 官方驱动适配、索引/事务/租约/回执/恢复管理、详细中文注释与真实副本集集成测试 | MongoDB 8.0.32；全量 test、mongodb/store/domain race、vet、build 均通过；无数据库测试跳过 | P4 应用服务与事件流 |
| 2026-09-23 | 按用户要求简化 P3：删除 store 接口、仓储转发及泛型 CRUD/事务；具体方法直接操作 MongoDB，更新中文注释、行为测试和调用文档 | 真实 MongoDB 全量测试、race、vet、build | P4 应用服务与事件流 |
| 2026-09-24 | P4 应用服务、运行生命周期/续租/取消/恢复接入、持久事件轮询、中文注释与行为测试 | 真实副本集全量 test、app/mongodb/store/domain race、vet、build 均通过，无数据库集成测试跳过 | P5 上下文基础能力 |
| 2026-09-24 | P5 上下文组装、工具配对/摘要覆盖、Token 预算与明确超限、P4 准备接入及中文注释/行为测试 | 全量 test、contextengine/app/mongodb/domain race、vet、build 通过；真实副本集测试未跳过 | P6 ADK 最小执行链路 |
| 2026-09-24 | P6 ADK/OpenAI 兼容执行、动态配置、完成检查点恢复、中文注释及行为测试 | 全量 test、ADK/app/contextengine/config/mongodb/domain race、vet、build 通过；真实 MongoDB 集成未跳过；真实提供方冒烟待配置 | 完成 P6 真实模型验收，随后 P7 |
| 2026-09-24 | P7 Gin/JWT、HTTP/SSE、启动装配、中文注释及行为测试 | 全量 test、HTTP/bootstrap/config/server/app race、vet、build 通过；真实副本集与本地模型端到端通过 | P8；P6 真实模型仍待配置 |
| 2026-09-24 | P8 Scope 工具注册、本地/MCP/A2A、ADK 工具循环、任务检查点交接、暂停交互及中文说明 | 全量 test、工具/协议/ADK/app/bootstrap/HTTP race、vet、build；真实副本集双任务交接通过；外部提供方待配置 | P9；P6/P8 外部冒烟待配置 |
| 2026-09-27 | P8 MCP/A2A 改用官方 Go SDK，移除自建 RPC/SSE，保留 HTTP 授权边界 | 全量 test（真实 MongoDB 副本集）、适配器/bootstrap race、vet、build 通过；MCP tasks 与 A2A SSE 游标限制已记录 | P9；外部提供方待配置 |
| 2026-09-27 | P9 任务观察/取消、SDK 原子接纳与分支恢复、启动扫描、部分交接补齐、不确定副作用屏障、中文注释与行为测试 | 全量 test（真实副本集）、app/ADK/MongoDB/bootstrap/HTTP race、vet、build；外部提供方待配置 | P10；P6/P8/P9 外部冒烟待配置 |
| 2026-09-27 | P10.1 压缩阈值/近期轮次/摘要模型配置、用户原文与工具关联保护、有界失败回退、租约/CAS 保存、中文注释与行为测试 | 全量 test（真实 MongoDB 8.0.32）、contextengine/config/ADK/MongoDB/app race、vet、build 通过；外部摘要模型待配置 | P10.2 容量、指标和追踪 |
| 2026-09-27 | P10.2 单实例容量/过载/满载幂等重放、固定类别指标与有界追踪、日志字段脱敏、恢复与 artifact 保留责任、中文注释及行为测试 | 全量 test（真实 MongoDB 8.0.32）、observability/config/app/ADK/MongoDB/tool/HTTP race、vet、build 通过；外部提供方仍待配置 | P10.3 整体验收与交付 |
| 2026-09-28 | P10.3 HTTP→ADK→A2A 任务→应用重启续接→SSE 重放整体测试、真实 TCP 提交故障、严格验收脚本、中文注释及部署/排障交付文档 | acceptance.ps1 全量 test（真实 MongoDB 8.0.32）、10 个关键包 race、vet、build 通过；仅外部模型/工具冒烟跳过 | 实际提供方及生产拓扑验收，项目保持未全部完成 |
