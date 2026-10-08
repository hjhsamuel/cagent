# MongoDB 结构维护入口

此目录是 MongoDB 集合名和 BSON 映射结构的统一定义位置。新增、修改存储字段时在此维护，适配器只处理查询、索引、编解码和业务转换；配置层只处理进程参数、模型校验与加解密。

| 集合 | 文档结构 | `data` 载荷 |
| --- | --- | --- |
| `models` | `Model`，嵌套 `ModelConfig`、`Thinking`、`EncryptedKey` | 无持久化信封 |
| `sessions` | `Document` | `domain.Session` |
| `runs` | `Document` | `domain.Run` |
| `messages` | `Document` | `domain.Message` |
| `events` | `Document` | `domain.Event` |
| `tasks` | `Document` | `domain.Task` |
| `task_deliveries` | `Document` | `domain.TaskDelivery` |
| `agent_checkpoints` | `Document` | `domain.Checkpoint` |
| `context_snapshots` | `Document` | `domain.ContextSnapshot` |
| `mutation_receipts` | `Document` | `store.MutationReceipt` |
| `run_leases` | `Lease` | 无持久化信封 |
| `clock` | `Clock` | 无持久化信封 |

业务集合沿用既有 `schema=1` 持久化信封和领域数据编码，保留 nil/空切片、二进制数据与整数的原始含义。领域类型定义仍属于领域层；新增存储查询字段必须在 `Document` 定义 BSON 标签，不能把持久化映射放回领域类型或进程配置中。`Document.Protocol` 是远端任务的工具协议，与已移除的模型配置 `protocol` 无关。

`responses.go` 集中定义拓扑探测、时钟和 failpoint 返回结构，它们是命令/聚合结果，不是业务表。

模型密钥的 `id` 是模型内稳定引用，轮换加密密钥时保留；`ciphertext`、`nonce` 均为独立 Base64 字符串，`version` 只标识环境 keyring 中的 AES 密钥版本；数据库不保存 AES 密钥材料。版本字符串参与 AES-GCM 附加认证数据，修改版本、nonce 或密文会导致解密失败。运行时严格要求新格式；旧合并密文通过 `modelkey -rotate -legacy` 显式迁移，详见 [模型配置](../../../docs/models.md)。

会话与分支检查点的信封 `model_id`/`api_key_id` 对应载荷的 ModelID/APIKeyID。会话仅绑定主对话，子调用绑定保存在分支检查点中；两者都只保存标识，不保存明文凭据。
