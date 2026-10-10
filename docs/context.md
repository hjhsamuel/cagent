# 上下文组装与基于 LLM 用量的摘要

`internal/contextengine.Builder` 负责模型输入选择、消息身份和工具配对校验；不访问数据库、不调用模型，也不计算 Token 预算。`app.NewContextPreparer` 负责作用域内读取历史与快照，并接入 `Options.Prepare`。

## 输入、顺序与信任边界

输入包含当前 Session、RunID、可信 System、持久 History、可选 Snapshot 和 PolicyVersion。固定系统约束来自可信装配配置，不能使用用户或工具文本填充。历史必须属于当前作用域/会话，消息 ID 唯一，序号连续；已验证快照允许通过作用域内的仓库窗口读取跳过已覆盖前缀，未覆盖尾部仍须连续。

当前 Run 必须包含 user 输入；之后不能出现其他 Run。持久历史只能使用 user、assistant 和 tool 角色，不能伪装为 system。摘要须属于当前作用域/会话，已持久化（Version>0）、覆盖水位有效、Summary 非空，且策略版本兼容。无摘要是正常情况；越界或非法快照明确报参数错误。

组装顺序为固定系统约束、资料信任边界说明、已有派生摘要、未覆盖或受保护的历史与当前输入。摘要以 user 角色发送，不能提升为系统指令；工具原文保持结构化角色和关联。

工具调用按 RunID+ToolCallID 配对，拒绝孤立、重复及错配的结果。完整工具调用/结果所在消息、未完成调用和当前 Run 消息始终受保护。旧 Run 已终止而结果未接纳时，补充本地终态的工具响应，不重放旧调用。不同 Run 可使用相同的提供方局部调用 ID。

原始历史只读，不删除或改写；消息、Part.Data 和快照通过深拷贝隔离，调用方修改结果不会影响其他请求。

## 装配与运行

构造 `contextengine.New()`，再调用 `app.NewContextPreparer(db, engine, app.ContextOptions{System: trustedParts, PolicyVersion: policyVersion})`。把返回函数赋给 `app.Options.Prepare`，再创建服务。准备函数读取 LatestSnapshot；兼容且带验证水位的摘要使用一致快照内的受保护前缀和连续尾部，否则按 128 条分页加载原始历史。

准备完成后才将 Messages 交给 Runtime；消息或工具关联错误、查询失败仍会阻止运行。取消和租约丢失遵循已有持久恢复规则。ADK BeforeModel 注入已组装的上下文，并保留 SDK 的工具声明与各轮工具历史；不会重新计算 Token。

输入不进行本地 Token 计数、模型窗口校验或裁剪，主模型与摘要都不发送 `max_tokens` 或 `max_completion_tokens`。响应中的 usage 保留为事件元数据；prompt_tokens 同事务随完整 assistant 消息持久化，用于下一轮准备时的摘要触发，不参与请求限制。请求超时、模型调用次数限制和工具输出大小限制继续生效。

## 依据 LLM 返回的用量触发摘要

`CAGENT_CONTEXT_COMPRESSION_ENABLED` 默认 true，`CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT` 默认 80，启用时为 1–100 的整数；模型的 `config.window_tokens` 必须填写其实际上下文上限（正整数）。在下一轮准备上下文时，比较最新 assistant 响应（含工具调用）报告的 `prompt_tokens`；达到主模型上下文上限的指定比例才尝试摘要；触发线向上取整到整数 Token。用量随消息持久化，重启后仍有效。缺失用量的旧消息或最新响应不触发摘要，不本地估算、不累加历史用量，也不沿用更早响应或摘要模型的用量。模型上下文上限只用于比例触发，不进行本地窗口校验或请求限制。

`CAGENT_CONTEXT_KEEP_RECENT_ROUNDS` 默认 2，启用时至少为 1，仅决定摘要范围。轮次按 user 消息计算，包含当前轮；当前 Run 的全部消息无条件保留。达到用量阈值后，历史还须超出近期保留范围，且前缀中存在可替换消息才生成摘要。

默认保留全部用户原文、系统约束、完整工具调用/结果、未完成调用和当前 Run，用摘要表示早期普通 assistant 消息。显式启用 `CAGENT_CONTEXT_ARCHIVE_COMPLETED` 时，完整已结束轮次的用户与工具消息可以由摘要表示；原文仍保存在数据库。归档策略使用独立 `/archive-v1` 策略版本。

兼容且已验证的快照使用旧摘要和新增前缀生成增量摘要；策略版本变化从原始历史重建。已覆盖同一水位时不重复调用摘要模型；增加近期保留轮数时不能沿用过度覆盖该范围的旧摘要。

摘要模型和 API key 独立随机选择；使用所选文档的地址和请求超时。历史编码为不可信 JSON 资料，以一条 user 消息发送，不携带工具声明，不把历史工具调用当作可执行指令。摘要采用非流式响应，SDK 自动重试关闭。

每次准备最多调用一次摘要模型，不按 Token 分段。摘要失败、空白、截断或无效响应时保留原输入或兼容旧快照；取消时返回错误，不保存迟到摘要，不调用主模型。摘要不再依靠 Token 或字符长度比较来决定是否保存。

## 快照保存、并发与恢复

`Prepared.NewSnapshot` 是待保存候选，携带旧快照 Version、Scope、SessionID、ThroughSequence 和 PolicyVersion。应用通过 `agent.Request.ContextSnapshot/ContextSessionVersion` 交接候选；模型运行时不会收到这些保存字段。准备只读，摘要在写锁外调用，运行持有者继续续租。

生成前应用在写锁内调用 `Database.SaveSnapshot`，同事务校验租约 Owner/Fence/有效期、Run.Version、读取时 Session.Version、最新快照 Version 及覆盖水位。成功后刷新 Run.Version，再调用主模型；保存失败不发主模型请求，结果未知时停止执行并遵循既有恢复规则。过时候选不能覆盖新历史，原始消息始终保留。

只有没有应用恢复记录的新 Run 才准备摘要上下文；已有检查点按原 Invocation/ToolCall 恢复，不修改待完成任务状态，不重新 Execute 已启动任务。

## 验证

单元测试覆盖长输入完整保留、系统与摘要顺序、近期轮次保护、用量阈值边界及最新响应缺失用量、工具配对、非法历史拒绝、失败回退不重试、取消、策略升级、深拷贝和并发隔离。本地 HTTP 测试通过真实 OpenAI SDK 验证主模型与摘要不发送 Token 限额，长输入仍能发送，usage 保留，以及截断和服务错误处理。

MongoDB 临时副本集测试覆盖跨分页历史、快照水位、当前输入唯一、候选保存的 Fence/CAS、连续轮次的增量摘要、取消后迟到摘要拒绝及原始历史不变。真实外部模型的接口兼容性和摘要质量仍需单独显式运行冒烟测试，见 [ADK 接入说明](adk.md)。
