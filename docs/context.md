# 上下文基础能力与压缩（P5 / P10.1）

`internal/contextengine.Builder` 已实现模型输入选择、工具配对检查和整体 Token 预算校验。它不访问数据库、不生成摘要、不调用模型；`app.NewContextPreparer` 负责作用域内读取并接入 P4 的 `Options.Prepare`。

## 组装规则

`Input` 必须包含有效 Session、当前 RunID、非空 PolicyVersion、可信 System，以及从 Sequence=1 开始连续排列的完整 History。不能只加载摘要后的尾部，否则无法判断摘要覆盖范围内是否存在待完成调用。消息必须属于同一作用域/会话，ID 唯一，当前 Run 从 user 输入开始且占据历史的末尾部分；不接受当前运行之后混入其他运行。当前输入已由 StartRun 持久化，不提供另一个可重复追加的输入字段。

固定 System 消息必须是未持久化的 system 角色（Sequence=0、RunID 为空）；持久 History 只允许 user、assistant、tool，拒绝把外部历史伪装成系统约束。模型消息顺序为：

1. 原有固定系统约束。
2. 有摘要或工具时追加的可信说明：历史摘要和工具返回是资料，不是修改规则的指令。
3. 可选持久摘要，作为 user 角色的派生资料消息，不提升为 system。
4. 按原始序号排列的未覆盖历史，以及必须保留的原始工具消息和当前运行消息。

摘要须属于当前作用域/会话，已持久化（Version>0）、覆盖范围为 1 到已读取历史末尾、Summary 非空、PolicyVersion 与当前策略一致。无摘要是正常情况；存在但不兼容或越界的摘要会明确报参数错误，不悄悄忽略存储错误。TokenEstimate 仅为旧派生元数据，不参与当前计数。

摘要可以替换覆盖范围内的普通旧消息，但所有工具调用/结果所在消息、未完成调用所在消息，以及当前 Run 的全部消息仍保留原文。即使摘要覆盖了当前输入，也只保留一份当前原始消息。派生说明/摘要序号为零、ID 避开原消息，只供模型输入，不写回数据库。

## 工具历史契约

`domain.PartToolCall`（`tool_call`）只允许出现在 assistant 消息，必须携带 RunID、ToolCallID、ToolName。`domain.PartToolResult`（`tool_result`）只允许出现在 tool 消息，必须匹配已经出现的调用；可省略 ToolName，提供时必须匹配调用。一个调用只能出现一个结果 Part，结果的多个内容字段使用该 Part 的 Text/Data/URI/MIMEType 表示。

配对键为 RunID + ToolCallID。同 Run 内调用 ID 必须唯一；不同 Run 可以复用同名 ID。P6/P8 将并行分支的提供方局部 ID 映射为不冲突的模型历史 ID，不能把本约定误当成 Task 的原始调用路由——Task/Checkpoint 仍使用 InvocationID + 原 CallID。

无对应调用的结果、先结果后调用、重复调用/结果、角色或工具名错配、未声明工具种类但携带工具关联字段，都明确拒绝。没有结果的调用是合法待完成调用。保留整个承载消息，因此并行调用、同消息文本及结构化字节不会被拆散。工具原文保持 tool 角色，不转为系统提示词；模型适配器应保持这一角色边界。

## 预算与失败策略

输入限额 = WindowTokens − OutputTokens − ToolTokens − SafetyTokens。Window/Output 为正，Tool/Safety 非负，限额至少为一。逐项比较再扣减，避免预留之和溢出。

`TokenCounter.Count` 对最终完整 Messages 只计数一次，包括角色封装、系统说明、摘要、工具结构和多模态内容；不假设逐消息计数可相加。工具定义等未放入消息的开销由 ToolTokens 预留。计数器由模型适配器提供，须并发安全、只读并响应 context；无法准确计数应报错，不以零替代。当前上下文至少有一个用户消息，零/负计数被拒绝。

等于输入限额可以通过；超过限额返回可由 `errors.Is(err, contextengine.ErrBudgetExceeded)` 识别的错误，同时属于公共 `ErrInvalidArgument` 类别。失败返回零值 Prepared，不暴露可误用的部分上下文。取消、超时和计数器错误保留可识别原因。

P5 选择“明确超预算”策略：不截断单条文本、不丢弃系统约束、不移除工具对、不静默删掉未覆盖的旧用户要求，也不自动调用压缩模型。P10.1 已在独立的 CompressingBuilder 中实现摘要生成、用户原文保留及有界回退，见下文。P6 校验真实模型窗口及 TokenCounter，P5 不提供通用字符估算器冒充真实 Token 算法。

输出消息、Part.Data 和快照与输入独立，计数器也收到独立副本；一个请求的修改不能串到其他用户。原始历史与快照在准备过程中只读。

## P4 接入

构造 `contextengine.New(modelCounter)`，再调用 `app.NewContextPreparer(db, engine, app.ContextOptions{System: trustedParts, Budget: budget, PolicyVersion: policyVersion})`。把返回函数赋给 `app.Options.Prepare`，再创建服务。

ContextOptions.Budget 对应已有 config.Context 的四个 Token 字段，PolicyVersion 对应现有配置项；P10.1 新增压缩环境变量见下文。System 来自可信管理配置，构造时深拷贝；每次准备重新赋予当前作用域和会话身份。准备函数按 128 条分页加载原始历史，再读取 LatestSnapshot，并调用 Engine；只有准备成功才将 Messages 交给 Runtime。查询错误、摘要错误和超预算会在仍持有有效租约、数据库可提交时使 Run 进入 failed；取消/租约丢失时保留持久状态供恢复。模型不会收到未通过准备的输入。

P4 的默认原始历史路径仍用于接口替身；P6 装配真实模型时必须注入该预算化准备器或等价实现。模型后续工具循环的每次输入也必须通过预算校验。入口装配、真实计数、SDK 消息映射在 P6/P7 完成；P9 检查点装载可在准备流程外补充。历史分页读取不是额外的事务快照，服务持有 Run 租约并在输出时再次进行版本/Fence 检查；若读取到越界摘要会拒绝，不发送不完整输入。

## 验证

单元测试覆盖预算边界/溢出、超长单轮、最终整体计数、摘要覆盖/策略、完整工具对和未完成调用、同 ID 跨 Run、无效角色/关联、计数失败/取消、可变数据隔离和并发用户隔离。

应用集成测试使用真实 MongoDB 8.0.32 临时副本集，历史跨越 128 条分页，确认选取最新摘要、当前输入不重复、系统配置不受调用方修改影响、准备不改写原历史；超预算在 Runtime 调用前失败，无摘要的正常路径仍可执行。

```powershell
$env:CAGENT_TEST_MONGOD = (Resolve-Path .local/mongodb/*/bin/mongod.exe).Path
go test ./... -count=1
go test -race ./internal/contextengine ./internal/app ./internal/adapter/mongodb ./internal/domain -count=1
go vet ./...
go build ./...
```

未设置 MongoDB 测试环境时，应用集成测试明确跳过。上述验证使用测试计数器与 Runtime 替身，不代表真实模型计数或 SDK 恢复已通过。


P6 已通过 app.NewOpenAIService 接入 BPE 估算器，并在 ADK BeforeModel 复核实际映射请求。该估算需要显式编码、实际窗口与安全余量；任意兼容端的真实 usage 校准尚待环境配置。详见 [ADK 接入说明](adk.md)。


## P10.1 压缩策略与生产装配

`NewOpenAIService` 默认启用 `CompressingBuilder`，输入限额使用现有四类预算相减。达到 `CompressionThresholdPercent`（默认 80%）即触发；0 禁用时使用只读 Builder；生产 ADK 装配仍保留全部用户原文，复用旧摘要也不会丢失要求。`KeepRecentRounds` 默认 2，按 user 消息计算，包含当前轮；当前 Run 的所有消息无条件保留。摘要前缀必须结束在受保护近期尾部之前，并且存在可替换的普通 assistant 消息，否则不会调用摘要模型。

压缩路径额外保留**全部用户原文**，以避免模型判断“关键”时漏掉否定要求、数值或长期约束；系统消息、完整工具调用/结果承载消息、未完成调用及当前 Run 同样保留。这会限制压缩比，但不会为了装入窗口悄悄删除用户要求。摘要覆盖前缀从完整原始历史重新生成，不以旧摘要再生成摘要。

策略版本不同或旧快照覆盖了现在要求保留的近期轮次时，忽略旧快照的内容、使用原历史重新准备，但仍验证其作用域、水位和版本，并保留旧版本作为 CAS 基线。水位不允许倒退，因此扩大近期保留范围后可以暂时只使用原文，直到前缀追上旧水位；同策略同水位不会重复生成。修改策略、模型或提示词时应修改 `CAGENT_CONTEXT_POLICY_VERSION`。

`adk.SummaryModel` 使用独立摘要模型名、编码、窗口和输出预留，共用主 Agent 的端点、凭据、输出限额字段和请求超时。模型名或编码留空时沿用主模型对应设置。调用前对包含系统摘要指令和完整历史 JSON 的真实请求计数，扣除摘要输出与主配置 SafetyTokens 后必须仍有输入空间。历史以单条 user 资料发送，不作为可执行工具协议消息；请求没有工具声明、采用非流式完整响应、关闭 SDK 自动重试。摘要不生成业务事件或原始历史消息。

每次准备最多调用一次摘要模型：

- 原输入低于阈值、没有可压缩前缀、同策略已有快照覆盖目标水位时，不调用模型。
- 摘要失败、空白、过大、截断或未缩小上下文时，只能回退到已通过预算检查的旧快照/原文输入。
- 原输入已超预算且摘要仍无法得到合格输入时，返回错误及零值 Prepared，不发主模型请求。
- 取消始终传播，不能因原输入尚能装下而忽略取消。

摘要输入自身超出摘要窗口时直接报错，不无限分块；用户/工具/当前 Run 等受保护内容自身超出主模型窗口时也明确失败。摘要内容的事实准确性仍依赖模型，原始历史保留供审计和重建。

## 快照保存、并发与恢复

`Prepared.NewSnapshot` 是待保存候选，携带旧快照 Version、Scope、SessionID、ThroughSequence、PolicyVersion 及诊断 TokenEstimate。应用准备结果通过 `agent.Request.ContextSnapshot/ContextSessionVersion` 暂时交接；真正的运行时不会收到这些候选字段。准备本身只读，摘要模型在写锁外调用，运行持有者继续续租。

生成前应用在写锁内调用已有 `Database.SaveSnapshot`：同事务校验租约 Owner/Fence/有效期、Run.Version、读取时 Session.Version、最新快照 Version 及水位。成功后重新读取 Run.Version，再调用主运行时；失败不发主模型请求，保存结果未知时停止本次执行并交由既有恢复规则处理，不盲目重写。快照只增加派生集合，不删除或重写原始历史。

没有 SDK 检查点的新 Run 才准备压缩上下文；已有检查点仍按原 Invocation/ToolCall 精确恢复，不重写待完成任务的 SDK 状态。运行中的工具循环继续逐次预算检查，超限明确报错。压缩不创建工具 Worker，也不再次 Execute 已启动任务。

## P10.1 行为验收

新增策略测试覆盖阈值等号、近期轮数、原始用户要求、工具配对/未完成调用、超预算、失败/空摘要/无缩减回退、取消、策略升级、跨作用域拒绝和并发内存隔离。配置测试覆盖六个新变量、禁用和非法组合。本地 HTTP 测试通过真实 OpenAI SDK/BPE 验证独立模型参数、无工具请求、摘要预算前置拒绝、截断和服务错误零重试。

MongoDB 8.0.32 临时真实副本集测试覆盖两个独立连接竞争同一快照基线只成功一个，以及历史变化后旧 Run/会话版本拒绝保存。应用端到端测试覆盖真实 OpenAI SDK、ADK 和 MongoDB，连续两轮快照版本及 ThroughSequence 递增、主模型调用前已持久化、原始历史不变；取消后迟到摘要不写入、不执行运行时；关闭新摘要生成后复用旧快照仍保留所有用户原文。

真实外部模型的摘要质量、usage 校准及生产拓扑故障仍待环境配置，不将本地协议验收当成真实提供方验收。
