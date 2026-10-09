# MongoDB 模型配置

服务在启动时从 `CAGENT_MONGODB_DATABASE` 的 `models` 集合加载模型快照，允许模型目录为空；模型缺失在创建会话选择模型或运行时解析已有绑定时返回错误。不设置默认模型；创建会话时由用户选择文档 `_id`，未选择则均匀随机抽取模型。文档的 `model` 才是发送给供应商的模型名称。已有模型的参数缺失或非法会终止启动；启动不校验 AES 密钥、密钥权重或 API key 密文，使用时严格校验，未知版本或认证失败返回错误，不回退到环境中的明文 API key。直接修改 MongoDB 后需重启服务生效；通过运维 HTTP 接口修改模型或轮换密钥，成功后本实例立即生效，已经构造的运行客户端在后续解析绑定时才使用新配置。

`models` 是部署管理员维护的全局配置，不使用租户/用户作用域，通过无需认证的运维 HTTP 接口管理，不通过租户业务 API 暴露。使用 MongoDB 自带的 `_id` 唯一索引；模型集合必须为普通集合且使用 simple 排序规则。

## 模型配置 HTTP API

所有模型管理接口无需认证；装配模型管理依赖后注册。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/debug/models` | 200，返回 `{"models":[...]}`，查询本实例当前目录；空目录返回空数组 |
| GET | `/debug/models/:modelID` | 200，查询一个模型；不存在返回 404 |
| PUT | `/debug/models/:modelID` | 新增或完整替换模型配置，200 返回保存后的配置 |
| DELETE | `/debug/models/:modelID` | 删除模型，成功返回 204；不存在返回 404 |
| POST | `/debug/model-keys/rotate` | 轮换 AES 加密密钥，详见下文 |

PUT 请求示例（`modelID` 来自 URL，请求体不包含 `id`）：

```json
{
  "model": "vendor-model-name",
  "provider": "GLM",
  "base_url": "https://example.invalid/v1",
  "api_keys": [
    { "id": "glm-key-1", "value": "供应商API密钥", "weight": 3 }
  ],
  "config": {
    "window_tokens": 32768,
    "request_timeout": "2m",
    "thinking": { "enabled": false }
  }
}
```

每个 key 必须指定模型内唯一的非空 `id`。`value` 是写入用明文，服务使用当前最高版本 AES 密钥自动加密，仅将密文保存到 MongoDB；新 key 必须提供 value。更新时省略 value 或设为 null，可保留该 ID 的已有密文并修改权重。查询与 PUT 响应中 api_keys 仅包含 `id/version/weight`，不返回 value、ciphertext 或 nonce；不返回 AES 密钥。响应中的 version 为只读信息，PUT 请求不接受此字段，更新时按上面的写入结构构造请求。

PUT 会校验模型参数、加密密钥、全部凭据及权重，至少一个 key 权重大于 0；无效配置返回 400，未知字段、多段 JSON 和空请求体返回 400，请求体超限返回 413。目录修改与 AES 轮换在本实例内串行执行，MongoDB 持久化成功后才发布内存配置；数据库失败时保留旧目录。覆盖和删除使用事务比较当前数据库文档与本实例旧快照，并发修改返回 409，需同步配置并重启加载后重试。新增同 ID 文档冲突也返回 409。多实例部署的其他实例须重启加载新配置；查询不直接读取外部修改后的数据库。

PUT 是完整替换，未保留的 key 会被删除。供应商 API key 轮换建议使用新 ID 添加新 key，把旧 key 权重设为 0 并保留到相关会话结束。同 ID 提供新 value 会使后续绑定解析使用新凭据；删除 key 或模型会使依赖它的已有会话明确失败。

## 文档结构

下面是持久化结构示例，地址、模型名和密文为占位；通过上述 PUT 接口添加模型时，密文由服务自动生成：

```json
{
  "_id": "example-model",
  "model": "vendor-model-name",
  "provider": "GLM",
  "base_url": "https://example.invalid/v1",
  "api_keys": [
    { "id": "glm-key-1", "version": "v1", "ciphertext": "BASE64_CIPHERTEXT", "nonce": "BASE64_NONCE", "weight": 3 },
    { "id": "glm-key-2", "version": "v2", "ciphertext": "BASE64_CIPHERTEXT", "nonce": "BASE64_NONCE", "weight": 1 }
  ],
  "config": {
    "window_tokens": 32768,
    "request_timeout": "2m",
    "thinking": {
      "enabled": true,
      "key": "thinking.type",
      "value": "enabled"
    }
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `provider` | 非空供应商分类名称，如 GLM、DeepSeek；不参与客户端选择，调用统一使用 OpenAI 兼容接口 |
| `base_url` | HTTP/HTTPS API 前缀，不允许 URL 内凭据、查询或片段 |
| `api_keys[].id` | 模型内唯一的稳定凭据标识；加密密钥轮换时保留，以支持已有会话 |
| `api_keys[].version` | 加密 API key 所用的 AES 密钥版本 |
| `api_keys[].ciphertext` | Base64 编码的 AES-GCM 认证密文，包含认证标签，不包含 nonce；无明文回退 |
| `api_keys[].nonce` | 独立 Base64 编码的 12 字节随机 nonce，每次加密重新生成 |
| `api_keys[].weight` | 非负整数；0 暂停新选择，已有会话仍可使用，至少一个为正；总和不可溢出 int64 |
| `config.window_tokens` | 必填正整数，填写供应商模型的实际上下文上限；仅用于摘要比例触发，无默认值 |
| `config.request_timeout` | 正 Go duration，如 `30s`、`2m` |
| `config.thinking` | 可选深度思考参数；`enabled=false` 或缺省时不发送 |

创建会话时按 `weight / 总权重` 随机选取 API key，会话只保存主对话的 `ModelID` 和 `APIKeyID`。后续所有主对话和工具轮次均使用这个绑定，服务重启后也不重新抽取。Subagent 每次新调用独立随机抽取模型及加权 API key，恢复时使用分支检查点保存的绑定，不覆盖会话。不会在失败后自动换 key 或重试。权重为 0 的 key 也必须是可解密的有效密文。

深度思考的 `key` 是供应商要求的 JSON 参数路径，可指定 `thinking.type`、`enable_thinking`、`reasoning_effort` 等；`value` 可以是 JSON 字符串、布尔值、数字或对象。只传递配置值，不按 `provider` 推断参数。路径不能覆盖模型、消息、工具、流控制和输出限额等核心参数。请按供应商实际协议设置。

摘要调用独立随机选择模型和加权 API key，不属于会话主对话。摘要依据最新 LLM 响应的 prompt_tokens 达到阈值触发，默认阈值为主模型 `config.window_tokens` 的 80%，通过 `CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT` 调整比例；近期轮数只决定摘要范围。不计算 Token 预算，也不设置输出 Token 限额。

## 加密密钥环境变量

| 环境变量 | 用途 |
| --- | --- |
| `CAGENT_MODEL_ENCRYPTION_KEY_V1` | 版本 `v1` 的 Base64 AES 密钥 |
| `CAGENT_MODEL_ENCRYPTION_KEY_V2` | 版本 `v2` 的 Base64 AES 密钥，按此规则继续增加版本 |

这些变量由服务调用轮换接口时生成并保存到 `.env`，用户无须提供初始密钥。启动、重启、查询和添加模型均不会生成 AES 密钥；首次添加模型前先调用轮换接口。已有密文仍须保留对应版本的密钥。每个变量只维护一个密钥，后缀为无前导零的正十进制整数（最大 uint64），对应数据库中的 `v<数字>` 版本；版本可以不连续。新密文自动使用数字最大的已配置版本，例如 V10 高于 V2。空值、非法版本、非法 Base64 或密钥长度错误会在加解密时导致失败，不回退到较旧版本；不要设置空密钥占位。

```dotenv
CAGENT_MODEL_ENCRYPTION_KEY_V1=BASE64_AES_KEY_V1
CAGENT_MODEL_ENCRYPTION_KEY_V2=BASE64_AES_KEY_V2
```

算法固定为 AES-256-GCM，密钥固定使用 32 字节（256 bit），以 Base64 编码保存；加载时要求 Base64 解码后的密钥恰好为 32 字节，拒绝其他长度。轮换生成随机 32 字节 AES 密钥。每次加密生成随机 nonce；版本字符串作为附加认证数据。数据库中不保存 AES 密钥，密钥版本可同时存在以支持分阶段轮换。不要记录完整环境、配置或明文 API key。

## 密文格式与轮换

初次配置时先启动服务（模型目录可为空），调用轮换接口生成 `v1`，再通过 PUT 模型接口提交供应商 API key，由服务使用该密钥加密。AES-GCM 使用随机 12 字节 nonce，并以版本字符串（如 v1）作为附加认证数据。ciphertext 为包含认证标签的密文，nonce 独立保存，两者均为 Base64；api_keys 中不保存明文。

轮换通过现有 server 的 HTTP 接口完成，不使用独立轮换命令。无需认证，调用：

```powershell
Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/debug/model-keys/rotate'
```

请求无须密钥或版本参数，无须 body。没有已有密钥时成功返回 `{"version":"v1"}`，后续依次返回 `v2` 等版本；响应不返回密钥。接口无需认证，装配密钥轮换依赖后注册。失败返回 500 `model_key_rotation_failed`，不回显密钥或数据库错误。

服务生成随机 32 字节 AES 密钥，使用大于现有版本的下一版本，将全部 API key 重新加密并保留稳定 ID、明文和权重。先通过临时文件替换工作目录 `.env`，保存全部旧版本和新版本，再以 MongoDB 事务更新模型密文，最后发布内存快照。原 `.env` 的其他配置和注释保留；文件无法写入或版本值冲突时不更新数据库。数据库失败时新增版本仍保留，旧密文仍可解密；同实例重试使用下一版本。轮换不修改进程环境变量。

同实例的轮换串行执行；数据库更新以旧密文数组为条件，拒绝覆盖并发修改。多实例部署需安排一个实例轮换，并在完成后将更新后的 `.env` 安全同步到其他实例再重启加载；清理旧版本前确认全部实例和备份都已迁移。数据库与文件系统不能组成共同事务，因此失败可能留下尚未使用的新版本。

加密密钥轮换保持供应商 API key 不变；若需轮换供应商 API key，先加密新 API key 并更新数组，将旧 key 权重设为 0 停止新会话选用；已有会话继续使用旧绑定。保留旧条目直到相关会话结束，删除后这些会话会明确失败，不随机更换凭据。

旧 `CAGENT_AGENT_MODEL`、`CAGENT_CONTEXT_SUMMARY_MODEL`、`CAGENT_CONTEXT_SUMMARY_TOKEN_ENCODING`、`CAGENT_AGENT_PROVIDER`、`CAGENT_AGENT_BASE_URL`、`CAGENT_AGENT_API_KEY`、`CAGENT_AGENT_TOKEN_ENCODING`、`CAGENT_AGENT_MAX_TOKENS_FIELD`、`CAGENT_AGENT_REQUEST_TIMEOUT` 不再读取。迁移时将模型、地址和超时等保留设置写入模型文档，并加密原明文 API key。

## 结构目录与旧数据迁移

所有 MongoDB 集合名和 BSON 结构集中于 internal/storage/schema。模型统一使用 OpenAI 兼容 Chat Completions，上下文不计算 Token 预算，不设置输出 Token 限额。模型文档必须提供 `config.window_tokens`；缺失或非正值会明确报配置错误。模型文档不再维护 protocol、token_encoding、max_tokens_field 和 output_tokens 字段；旧文档中的这些字段可删除，读取时会忽略，HTTP PUT 请求不再接受。数据库只保存稳定 ID、密文、nonce、版本标识和权重，具体版本的 AES key 由独立环境变量 `CAGENT_MODEL_ENCRYPTION_KEY_V<正整数>` 提供。server 自动加载当前工作目录的 .env，也可由启动环境注入这些变量；已有环境变量优先。

旧版将 nonce 拼接在 ciphertext 前的记录须预先迁移为独立字段；当前运行时和 HTTP 轮换拒绝缺失 nonce 的记录。

## 会话选择

`POST /api/v1/sessions` 可提交 `{"model_id":"example-model"}`。不提交请求体、提交 `{}` 或空 model_id 时，在创建会话时随机选择一次。不存在的模型返回 404。会话响应包含 model_id，不暴露 API key 或凭据标识。

模型与 API key 的持久引用只限定主对话，子调用的选择记录在各自检查点。模型或已绑定 key 从目录移除时明确报错，避免恢复期间切换身份。缺少 key id 的旧条目使用版本、nonce、密文的摘要作为稳定标识，HTTP 轮换时会保留此标识；不要手工轮换密文而丢失原 ID。旧会话在首次主对话时通过事务补齐一次随机绑定。缺少绑定的旧子分支检查点拒绝不确定恢复。
