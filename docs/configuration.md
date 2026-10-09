# 配置约定（P1–P9）

`internal/config` 已实现默认值、环境变量加载、类型解析与集中校验。它不连接数据库、不加载模型，也不监听 HTTP。当前 `cmd/server` 通过 LoadFromEnv 加载进程配置，校验 HTTP 配置后装配日志与数据库，再从 MongoDB 加载模型快照。模型文档、加密和迁移说明见 [MongoDB 模型配置](models.md)。

## 加载顺序与入口

1. `Defaults()` 返回独立的默认配置，不读取环境；MongoDB URI 留空，必须补齐；模型目录由启动装配从 MongoDB 加载，不设置默认模型。
2. `Load()` 使用进程环境覆盖默认值，`LoadFromEnv(lookup)` 可使用调用方提供的稳定环境快照。只读取表中已知键，忽略其他环境变量。
3. 先独立加载并校验日志选项，再解析其余配置的时间和整数，最后调用 `Config.Validate()` 检查完整配置。直接构造或修改配置也必须显式调用 Validate。

`server` 入口通过 `github.com/joho/godotenv/autoload` 自动加载当前工作目录的 `.env`，优先级是 **默认值 < `.env` < 已设置的环境变量**。`.env` 不存在时继续使用环境变量；autoload 会忽略加载错误。dotenv 文件支持引号、注释及变量引用展开。未设置与空字符串不同：未设置保留默认值，显式空字符串会覆盖并报错。合法的零值不会触发默认值回填。配置包自身只读取环境变量，字符串原样保留，不隐式 trim、不展开 `${VAR}`，不读取文件或命令行参数。

任何加载错误都返回零值 Config，不能使用半成品配置。校验按固定顺序返回首个错误，不聚合所有失败。加载不修改进程环境、没有可变全局配置；应在启动时加载，随后作为只读值传给组件。

## 环境变量

API 登录接口 `POST /api/v1/auth/login` 无需鉴权，不校验账号密码，每次生成随机身份 JWT。`CAGENT_HTTP_LOGIN_TOKEN_TTL` 默认 `1h`，允许 `1s` 到 `24h` 的整秒有效期。JWT 签名沿用 `CAGENT_HTTP_JWT_SECRET`，不需要配置用户名、密码或固定身份。详见 [登录与 JWT 签发](http.md#登录与-jwt-签发)。

| 环境变量 | 错误字段路径 | 默认值 | 约束 |
| --- | --- | --- | --- |
| `CAGENT_LOG_LEVEL` | `logging.level` | `info` | trace/debug/info/warn/error，接受大小写与 warning 别名 |
| `CAGENT_LOG_PATH` | `logging.path` | `/app/logs/cagent.log` | 非空白文件路径，不接受 NUL 或目录形式 |
| `CAGENT_LOG_SIZE` | `logging.size` | `50` | 正整数，单位 MiB，转换 int64 字节数不溢出 |
| `CAGENT_LOG_ROLL` | `logging.rolls` | `3` | 非负备份数量；0 不限制数量 |
| `CAGENT_HTTP_ADDRESS` | `http.address` | `127.0.0.1:8080` | host:port，数字端口 1–65535 |
| `CAGENT_HTTP_SSE_HEARTBEAT` | `http.sse_heartbeat` | `15s` | 正时间间隔 |
| `CAGENT_HTTP_SHUTDOWN_GRACE` | `http.shutdown_grace` | `30s` | 正时间间隔 |
| `CAGENT_MONGODB_URI` | `mongodb.uri` | 无，必填 | 非空白；可能含凭据 |
| `CAGENT_MONGODB_DATABASE` | `mongodb.database` | `cagent` | 非空白 |
| `CAGENT_AGENT_NAME` | `agent.name` | `cagent` | 非空白 |
| `CAGENT_TASKS_POLL_INTERVAL` | `tasks.poll_interval` | `2s` | 正时间间隔 |
| `CAGENT_TASKS_OBSERVATION_TIMEOUT` | `tasks.observation_timeout` | `30s` | 正时间间隔，仅限制单次观察 |
| `CAGENT_TASKS_RECONNECT_BACKOFF` | `tasks.reconnect_backoff` | `1s` | 正时间间隔 |
| `CAGENT_CONTEXT_POLICY_VERSION` | `context.policy_version` | `v1` | 非空白 |

时间使用 Go duration 语法，如 `500ms`、`1.5s`、`1m30s`；不接受无单位正数、空值、溢出、零或负数。整数使用十进制并受当前平台 int 范围约束，不接受小数、十六进制或溢出。

HTTP 可使用 `:8080`（所有网卡）、`localhost:8080`、`[::1]:8080` 或含 zone 的 IPv6 地址。主机名限 ASCII 标签，端口不接受服务名或 0。这里只检查形状，不解析 DNS 或探测端口，无法证明地址属于本机或可以监听。

上下文不计算 Token 预算，不设置模型输出 Token 限额。ObservationTimeout 不限制远端任务寿命；轮询间隔、观察超时和重连等待之间没有额外大小关系。

## 错误与安全边界

错误统一归为 `apperrors.ErrInvalidArgument`，可通过 `errors.As` 提取 `*apperrors.Error` 的字段路径及安全说明。标准数字/时间/地址解析错误可能携带输入值，本模块不保留这些原始原因；输出及原因链均不回显输入。不要把 Config、MongoDB.URI 或整个进程环境写入日志，也不要直接序列化为响应。

MongoDB 配置只要求 URI 和 Database 非空白，实际连接、认证、数据库名称限制及事务拓扑由 P3 适配器验证。模型文档的 Provider 只用于分类，不含 protocol 字段；调用统一使用 OpenAI 兼容接口，加载后校验 API 地址和超时；密钥池与 AES 密钥延迟到使用时校验。配置成功不代表实际模型可用；APIKey 与 MongoDB URI 一样禁止整体打印。

## 示例及后续接入

完整键清单见根目录 [`.env.example`](../.env.example)，其中包含无凭据的本地 MongoDB 地址，无须填写 AES 密钥。首次启动后调用 HTTP 轮换接口，由服务生成默认 32 字节密钥并保存到工作目录 `.env`，再添加模型。启动和重启只加载已有密钥；轮换同步保存新旧版本。可由部署环境设置其他变量，或复制为 `.env` 后填写配置，并从该文件所在目录启动程序。示例结构已通过加载行为测试，未执行真实连接。

P6 阶段配置项已扩展；当前完整清单包含下述 P7 配置。P1.4 使用 Logrus 默认实例及 Lumberjack 文本文件轮转，CAGENT_LOG_FORMAT 已移除，详情见 [日志约定](logging.md)。日志选项可单独加载。P6 模型错误不展开敏感 SDK 诊断，但这不等于全局敏感日志过滤已完成。动态配置按服务实例快照使用，详见 [ADK 接入说明](adk.md)。

PolicyVersion 用于检查持久摘要与当前策略是否兼容。app.NewContextPreparer 组装可信系统约束和会话历史，校验消息与工具关联；自动摘要依据最新 LLM 报告的 prompt_tokens 达到阈值触发，失败时保留原输入。详见 [上下文说明](context.md)。


## P7 HTTP 与 JWT 配置

实际 server 入口加载完整配置，并调用 `HTTP.ValidateServer` 检查认证与传输限制；离线存储/模型组件仍可在不配置 JWT 时使用 `Config.Validate`。

| 环境变量 | 默认值 | 约束 |
| --- | --- | --- |
| `CAGENT_HTTP_JWT_SECRET` | 无 | HS256 秘密，至少 32 字节，不得写入日志 |
| `CAGENT_HTTP_WRITE_TIMEOUT` | `10s` | 单次 SSE 写入/flush 截止时间，必须为正 |
| `CAGENT_HTTP_MAX_BODY_BYTES` | `1048576` | 请求体上限，必须为正 |

`CAGENT_HTTP_SSE_HEARTBEAT` 与 `CAGENT_HTTP_SHUTDOWN_GRACE` 现已接入实际服务。JWT 认证固定使用 HS256，只需配置 `CAGENT_HTTP_JWT_SECRET`。JWT 声明、接口、SSE 信封与部署边界见 [HTTP 与启动装配](http.md)。

## P8 工具配置

`CAGENT_TOOLS_FILE` 默认为空，表示禁用工具；非空时指向可信 JSON 清单，由 bootstrap 在启动时读取并校验。清单定义完整 tenant/user Scope、连接/工具白名单、凭据环境引用、单次超时、输入输出字节限制和模型循环次数。config.Load 只加载路径，不执行文件/网络 I/O。详见 [工具配置与边界](tools.md) 和 [清单示例](tools.example.json)。




## P9 任务观察与恢复

现有三个 Tasks 配置已接入生产装配，无新增环境变量。PollInterval 控制正常维护和持久 Follow 周期；ObservationTimeout 分别限制每次订阅、查询和取消请求，不限制远端任务寿命；ReconnectBackoff 控制观察失败/维护冲突退避，并作为启动恢复扫描的间隔。每 Run 每页最多 32 个任务、8 个并发网络观察，恢复扫描每页 64 个候选。HTTP 生命周期不拥有这些任务，服务关闭仅停止本地跟踪。语义与故障处理见 [长任务与恢复](tasks.md)。


## P10.1 压缩与摘要模型配置

| 环境变量 | 默认值 | 约束及用途 |
| --- | --- | --- |
| `CAGENT_CONTEXT_COMPRESSION_ENABLED` | `true` | 是否依据 LLM 返回的 prompt_tokens 生成摘要 |
| `CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT` | `80` | 最新 assistant 响应报告的 prompt_tokens 占主模型 config.window_tokens 的触发百分比，启用时为 1–100；缺失用量不触发 |
| `CAGENT_CONTEXT_KEEP_RECENT_ROUNDS` | `2` | 启用时至少 1，包含当前用户轮次；全部用户原文和工具消息额外保留 |

沿用 `CAGENT_CONTEXT_POLICY_VERSION`（默认 v1）标记生成策略，修改摘要模型、提示词、保留规则时应提升版本。版本变化从原始历史重建，不继续使用不兼容旧摘要。摘要模型和 key 独立随机选择，从所选 MongoDB 文档加载 Provider、地址、加权密钥池和超时。

最新 LLM 响应的 `prompt_tokens` 达到主模型 `config.window_tokens` 的 `CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT` 百分比，且历史超过 `CAGENT_CONTEXT_KEEP_RECENT_ROUNDS`、存在可替换的早期消息时，在下一轮准备中尝试一次摘要；兼容快照使用增量摘要，失败或空摘要时回退到旧快照/原文输入，不分段或重试。已移除的 Token 预算与固定 Token 阈值环境变量不再读取。完整环境键清单以 .env.example 为准；归档选项及维护期限见 [修复说明](remediation.md)。

## P10.2 容量与运维入口

| 环境变量 | 默认值 | 校验与用途 |
| --- | --- | --- |
| `CAGENT_CAPACITY_RUNS` | 64 | 1–100000，限制生成阶段，等待与维护释放槽位 |
| `CAGENT_MAINTENANCE_WORKERS` | 32 | 1–100000，独立限制本地任务维护 |
| `CAGENT_CAPACITY_MODELS` | 16 | 1–100000，主模型与摘要模型共享，包含整个流读取过程 |
| `CAGENT_CAPACITY_OBSERVATIONS` | 32 | 1–100000，跨 Run 的后台任务观察上限 |
| `CAGENT_HTTP_MAX_SUBSCRIPTIONS` | 256 | 1–100000，HTTP 服务的 SSE 订阅上限 |

四个容量值不接受显式空值或零。过载立即拒绝，无新增内存等待队列；具体失败、幂等重放、背压及恢复语义见 [运行保障与保留责任](operations.md)。工具输出大小继续由可信工具清单控制，不新增重复配置。运维路由不提供配置查看，不返回凭据、用户内容或错误正文。

## 模型加密与加载

服务启动时无须配置 AES 密钥；仅调用 `POST /debug/model-keys/rotate` 时由服务自动生成默认 32 字节密钥。服务将其以 `CAGENT_MODEL_ENCRYPTION_KEY_V<正整数>` 保存到 `.env`，重启后加载，每个变量只保存对应版本的 Base64 AES 密钥，新密文自动使用最大数字版本。首次添加模型前须先调用轮换接口；已有模型须保留能解密旧密文的密钥。算法固定为 AES-GCM，支持多版本密钥并存；完整文档和迁移步骤见 [MongoDB 模型配置](models.md)。模型文档不再包含 Token 编码、窗口或输出预算。
