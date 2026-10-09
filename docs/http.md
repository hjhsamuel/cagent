# HTTP、JWT 与启动装配（P7）

## 启动与身份

`go run ./cmd/server` 自动读取当前工作目录的 `.env`（已有环境变量优先），加载完整配置，校验 HTTP 认证配置，初始化日志，连接支持事务的 MongoDB 并初始化索引，装配预算化 ADK/OpenAI 服务后监听 HTTP。启动失败返回非零退出码。模型提供方仍需部署者设置可用地址、密钥、模型名和匹配的 Token 编码。

认证采用 `Authorization: Bearer <JWT>`，固定 HS256。密钥至少 32 字节，应由部署方生成随机秘密；支持本服务登录签发，也兼容可信身份服务签发以下声明：

```json
{"sub":"user-123","tenant_id":"tenant-123","exp":2000000000}
```

验证外部令牌只需配置 `CAGENT_HTTP_JWT_SECRET`。必须验证 HS256 签名和 exp；存在 nbf 时也验证。iss 和 aud 可省略，服务不校验这两个声明。sub 与 tenant_id 非空，分别成为 UserID 和 TenantID；客户端普通头、JSON 身份字段不能覆盖它们。不接受 URL 查询参数中的令牌。HS256 密钥仅供服务和可信签发方持有，通过部署环境传入。API 登录每次生成新的随机用户和租户身份；外部令牌的身份由可信签发方确认。

## 登录与 JWT 签发

`POST /api/v1/auth/login` 注册在 api 路由组中，在 JWT 中间件之前注册，无需已有 token。接口仅生成 token，不读取请求体，不校验账号密码。

```dotenv
CAGENT_HTTP_LOGIN_TOKEN_TTL=1h
```

有效期默认 1h，支持 1s 到 24h 的整秒 duration。原 `CAGENT_HTTP_LOGIN_USERNAME`、`CAGENT_HTTP_LOGIN_PASSWORD_HASH`、`CAGENT_HTTP_LOGIN_USER_ID` 和 `CAGENT_HTTP_LOGIN_TENANT_ID` 已移除，不再读取。

请求：

```http
POST /api/v1/auth/login
```

成功返回 200：

```json
{"token":"<JWT>","token_type":"Bearer","expires_in":3600}
```

JWT 使用 `CAGENT_HTTP_JWT_SECRET` 以 HS256 签名，`sub`、`tenant_id`、`jti` 各使用独立随机 UUID v4，尽量避免重复，另包含签发时间 `iat` 和过期时间 `exp`。响应设置 `Cache-Control: no-store`。后续业务请求使用 `Authorization: Bearer <token>`；登录 token 不具备 `/debug` 运维权限。

每次调用都生成新身份，需保存并复用原 token 才能访问原作用域的会话。请求体中的账号、密码或身份字段不参与处理。原 `/debug/auth/login` 已移除，返回 404。不提供刷新 token 或注销接口。

## 接口

除登录外，`/api/v1` 端点要求 JWT，资源访问均经过应用层 Scope；越权和不存在均返回 404。健康检查和登录无需认证；`/debug` 运维端点无需认证。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/healthz` | 200，进程 HTTP 存活 |
| GET | `/readyz` | 检查 MongoDB primary 可达；关闭或依赖不可用返回 503 |
| POST | `/api/v1/auth/login` | 无需鉴权或请求体，200 返回随机身份 JWT |
| POST | `/api/v1/sessions` | 可提交 {"model_id":"模型文档ID"}；省略模型时随机选择，201 返回会话 |
| GET | `/api/v1/sessions/:sessionID` | 200 返回会话 |
| POST | `/api/v1/sessions/:sessionID/runs` | `{"text":"你好"}`，202 返回 Run |
| GET | `/api/v1/runs/:runID` | 200 返回持久运行状态 |
| POST | `/api/v1/runs/:runID/cancel` | 204；取消已提交后才返回 |
| GET | `/api/v1/runs/:runID/events` | 持久事件 SSE 重放及跟随 |
| GET | `/api/v1/tasks/:taskID` | 200，作用域内持久任务状态/进度/结果，不公开远端句柄 |
| POST | `/api/v1/tasks/:taskID/cancel` | 202，取消意图已持久化，不代表远端已取消 |
| POST | `/api/v1/tasks/:taskID/resume-observation` | 202，恢复隔离任务的原句柄观察，不重开终态 Run |

提交 Run 可携带 `Idempotency-Key`，相同作用域/会话/键/输入返回原 Run，不重复调用模型；同键不同输入或同会话已有其他活动 Run 返回 409。P7 只接受文本用户输入，不开放客户端自选角色、工具调用或服务端系统约束。不做全面业务参数校验；保留 JSON 解码、单个 JSON 值、请求体大小及游标解析等必要边界。

会话 DTO 包含 `id/agent_id/model_id/active_run_id/version/created_at/updated_at`，model_id 对应 MongoDB models 文档 _id；API key 按权重在创建时抽取，与模型一起固定用于后续主对话，子调用独立选择。Run DTO 包含 `id/session_id/status/version/created_at/updated_at`。不直接序列化领域对象，不暴露 Scope、幂等键、租约或检查点。P9 已接入任务查询/取消，使用显式 DTO 隔离内部句柄和检查点。

P8 已增加 JWT 保护的 `POST /api/v1/tasks/:taskID/input`（`{"text":"回复"}`）与 `POST /api/v1/tasks/:taskID/authorization`（`{"text":"继续","credential_ref":"已配置引用"}`）。仅使用本地 Task ID，应用按认证 Scope 读取原调用、确认远端暂停状态后补充原任务；拒绝客户端注入 Scope/远端句柄。202 只代表提供方接纳。未配置工具、不支持交互返回 501；状态冲突返回 409；无权限/不存在返回 404。P9 持续观察并在结果原子接纳后恢复模型生成。详见 [工具说明](tools.md)。

错误格式为 `{"error":{"code":"not_found","request_id":"..."}}`。常用状态：400 参数/JSON/游标无效，401 认证失败，404 不存在或越权，409 冲突，410 游标过期，413 请求体过大，501 能力不支持，503 不可用，500 内部错误。错误不回传底层数据库、JWT 或 SDK 诊断。所有请求由服务生成 `X-Request-ID`，不相信客户端提供的关联 ID；访问日志显式记录关联 ID、匹配路由、状态以及会话/Run ID。panic 在提交响应前转换为安全 500，提交后只能结束响应。

## SSE 行为

共享本机通知和数据库 Change Stream 唤醒读取，空闲时每个 Run 只共享一次低频水位查询；不为每个连接执行高频空事务。任务退出、新增 DTO 字段和格式升级详见 [修复说明](remediation.md)。

`Last-Event-ID` 是已消费的非负序号；未提供时为 0。先验证作用域及游标，再提交 SSE 响应头，因此历史已清理时可以返回 JSON 410，未来游标返回 400。随后沿相同持久游标分页，终态水位消费完毕即关闭连接；不跳过追赶期间的新事件。

```text
: connected

id: 2
event: message.delta
data: {"run_id":"...","sequence":2,"data":"...base64...","created_at":"..."}

```

data 行是 JSON 信封；其中 `data` 字段为原始 Event.Data 的 base64，避免假定所有领域事件载荷都是有效 JSON；空载荷省略该字段。P6 消息载荷解码后是 JSON。客户端按 Run 内 sequence 去重，保存成功消费的最后序号。`: heartbeat` 是注释，不占用持久序号。使用 fetch 流式读取或支持 Authorization 头的 SSE 客户端；浏览器原生 EventSource 无法直接设置该头。

订阅只缓存应用层一页（生产 128 条）与当前交付事件，无无限队列。只有 handler 写响应，心跳和事件串行；每次写与 flush 使用可配置截止时间。不读数据的 TCP 客户端会被断开。连接取消仅停止订阅，不改变 Run；只有取消接口持久化取消意图。流开始后发生数据库故障或并发清理只能结束连接，客户端用已消费游标重连，不能把缺少终止事件的 EOF 当作运行完成。

## 关闭与边界

SIGINT/SIGTERM 触发：撤销就绪 → 取消 HTTP 请求上下文（使 SSE 退出）→ 关闭监听并等待 handler → 取消并等待应用运行 → 关闭数据库 → 关闭日志。HTTP/应用共享关闭宽限期，数据库清理另有有界超时；超时强制关闭 socket 并返回失败。关闭不写成用户取消，未完成 Run 保留供启动恢复扫描处理。P9 的独立管理连接扫描未完成 Run 与未结算任务，停止应用后关闭；没有独立工具 Worker。

生产 HTTP 设置：请求头读取 5 秒、请求读取 15 秒、空闲连接 60 秒、头上限 32 KiB；SSE 不设置整条流的总写时限。服务租期 30 秒、续租轮询 1 秒；持久事件由共享通知唤醒，每个有订阅的 Run 每 3 秒共享一次水位后备检查。就绪检查只验证当前数据库依赖，不代表模型提供方已通过真实冒烟。

行为测试覆盖 JWT 无效/过期/错误算法/未生效/缺失身份及可选签发方/受众、可信作用域、DTO、错误映射、panic、请求体限制、重放、心跳、终态、真实 TCP 慢客户端、关闭超时与端口释放。设置 `CAGENT_TEST_MONGOD` 后，真实副本集测试覆盖 JWT/Gin → ADK/OpenAI SDK → MongoDB → SSE，以及用户/租户隔离、断连后完成、幂等、冲突、取消与清理水位；模型提供方使用受控本地 HTTP，真实外部模型验收仍属 P6 待配置项。


P9 Task DTO 包含 `id/run_id/tool_call_id/agent_id/invocation_id/parent_invocation_id/status/progress/result/cancel_requested_at/observation_error/applied_at`，可选字段为空时省略。Part 字段为 `kind/text/mime_type/uri/data`，结果字段为 `tool_call_id/parts/error`；不序列化整个领域对象。任务取消 202 只表示意图已保存，不支持远端取消时仍保留最终查询结果；详见 [长任务与恢复](tasks.md)。

## P10.2 补充：容量与观测入口

新 Run 或 SSE 满载返回 `503`、`error.code=overloaded`、`Retry-After: 1`。运行容量在落库前预留，拒绝不创建输入或 Run；满载时同键同输入的已提交请求仍可重放。SSE 满载在流头前拒绝，断连/慢消费者退出后归还槽位，不取消 Run。

所有请求返回服务生成的 `X-Trace-ID`。默认注册 `GET /debug/metrics` 和 `GET /debug/traces`，无需认证。指标与追踪不包含用户正文，追踪只保留本实例最近 256 条已结束 span。详细契约见 [运行保障](operations.md)。


## 运维密钥轮换

模型配置管理同时提供 `GET /debug/models`、`GET /debug/models/:modelID`、`PUT /debug/models/:modelID` 和 `DELETE /debug/models/:modelID`，无需认证，成功写入后本实例立即生效。PUT 接受完整模型参数和写入用 API key 明文，由服务自动加密；查询只返回 key 的 ID、版本和权重，不返回明文或密文。请求结构、错误语义及会话绑定影响见 [模型配置 HTTP API](models.md#模型配置-http-api)。

`POST /debug/model-keys/rotate` 在现有 HTTP 服务内执行 AES 密钥轮换。接口无需认证；装配密钥轮换依赖后注册。请求无须 body，成功返回 `{"version":"v2"}`，失败返回 500 `model_key_rotation_failed`。接口生成新版本 AES 密钥，先写入工作目录 `.env`，再事务更新 MongoDB 密文并发布内存目录；旧版本、API key 明文、稳定 ID 和权重保留。细节和多实例操作见 [模型配置](models.md)。
