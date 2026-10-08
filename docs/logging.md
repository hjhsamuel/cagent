# 日志约定（P1.4）

当前使用 Logrus v1.10.2 默认实例和 Lumberjack v2.2.1 文件轮转。业务无需实例化 logger，直接调用 `logrus.Info`、`WithFields`、`WithError`。P10.2 已安装安全 formatter：字段采用白名单，错误转换为稳定类别，不安装 Hook。Message 必须使用代码中的固定文本。

## 初始化与文件输出

启动时调用 `observability.Init(config.Logging)`，验证后配置已有标准实例，不调用 logrus.New。必须在业务 goroutine 启动前完成；不支持并发重配。初始化失败不修改已有全局配置。

| 配置 | 环境变量 | 默认值 | 规则 |
| --- | --- | --- | --- |
| Level | `CAGENT_LOG_LEVEL` | `info` | trace/debug/info/warn/error，接受大小写与 warning 别名；拒绝 fatal/panic |
| Path | `CAGENT_LOG_PATH` | `/app/logs/cagent.log` | 非空白文件路径，拒绝 NUL、目录形式，不隐式 trim |
| Size | `CAGENT_LOG_SIZE` | `50` | 单位 MiB，正整数，换算为 int64 字节数不能溢出 |
| Rolls | `CAGENT_LOG_ROLL` | `3` | 非负备份数量；0 明确表示不限制数量 |

保持用户定义的 `CAGENT_LOG_ROLL` 单数键名。格式固定为无颜色文本，时间格式为 `2006-01-02 15:04:05`。`CAGENT_LOG_FORMAT` 已移除。配置校验不探测权限或磁盘空间；Lumberjack 在第一次写入时打开或创建文件，Init 成功不等于文件可写。部署时应设置适合本机的 Path。

文件超过 Size 后轮转，使用本地时间命名备份并异步 gzip 压缩。Rolls 约束备份数，不包含活动文件。日志同步写入，保留 Logrus 的写入锁；后台备份压缩由 Lumberjack 执行。

## 在输出位置显式关联

```go
// 启动装配执行一次。
if err := observability.Init(cfg.Logging); err != nil {
    return err
}
defer observability.Close()

logrus.Info("process.starting")

// 每次调用使用本次操作的 ID，不在全局 logger 中保存当前用户或请求。
logrus.WithFields(logrus.Fields{
    "request_id": requestID,
    "session_id": sessionID,
    "run_id": runID,
    "task_id": taskID,
    "invocation_id": invocationID,
}).Info("task.observation_started")

logrus.WithFields(logrus.Fields{
    "run_id": runID,
    "task_id": taskID,
}).WithError(err).Warn("task.observation_failed")
```

没有的 ID 不添加。每个 goroutine 在输出位置建立自己的字段映射，不并发修改共享 map 或 Entry.Data。全局 logger 只保存配置，不保存关联字段。已删除 WithCorrelation/context 自动关联机制；仅调用 WithContext 不会输出 ID。

入口、HTTP、运行维护和恢复扫描已有生产日志调用。关联 ID 在输出位置显式添加，不从请求隐式抽取其他字段。

## 生命周期与实现范围

正常骨架入口记录启动、提示和退出，返回 0，并关闭文件。配置无效时向 stderr 报告 `process.start_failed` 并返回 1，不回退到另一个默认日志文件。info 生命周期记录仍受级别阈值过滤。

`observability.Close()` 仅关闭当前 Lumberjack 文件输出，不关闭其他组件注入的 writer。须先停止日志写入，再关闭或重新初始化；关闭后继续写入会重新打开文件。Close 不等待异步压缩完成。实际服务的信号与优雅关闭在 P7 装配。

P10.2 使用字段白名单和错误类别转换；丢弃未知字段与 tenant/user，未知错误仅输出 internal_error，不展开原因链。Message 不允许拼接用户或提供方原文；这不是任意自然语言的秘密检测器。完整边界见 [运行保障](operations.md)。日志写入失败由 Logrus 报告至 stderr，业务日志方法不返回写入错误。公共错误包自身的安全文本约定保持不变。

测试使用临时文件，覆盖配置默认值/覆盖/非法值/溢出、级别过滤、默认实例身份、无效初始化不改变配置、并发显式关联、真实轮转与 gzip 内容、入口生命周期与失败退出；并发相关测试通过 race 检查。
