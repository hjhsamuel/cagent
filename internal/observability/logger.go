// Package observability 配置 Logrus 默认实例及 Lumberjack 文件轮转。
// 业务直接使用 Logrus API，在输出位置通过 WithFields 显式添加关联 ID。
package observability

import (
	"time"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Init 配置 Logrus 已创建的标准实例，不创建或返回另一份 logger。
// 应在启动 goroutine 之前调用一次；业务直接使用 logrus.Info/WithFields/WithError。
// 验证失败不修改全局配置。多项设置不是一个原子事务，不支持运行期间并发重配。
// 保留 Logrus 输出锁，安装字段白名单与错误类别 formatter，不从 context 隐式提取关联。
// 输出文件在首次写入时打开；Init 不保证文件可写。重新初始化前须先停止日志并 Close。
func Init(options config.Logging) error {
	if err := options.Validate(); err != nil {
		return err
	}

	logrus.SetOutput(&lumberjack.Logger{
		Filename:   options.Path,
		MaxSize:    options.Size,
		MaxBackups: options.Rolls,
		LocalTime:  true,
		Compress:   true,
	})

	level, _ := logrus.ParseLevel(options.Level)
	logrus.SetLevel(level)

	formatter := &logrus.TextFormatter{
		DisableColors:   true,
		FullTimestamp:   true,
		TimestampFormat: time.DateTime,
	}
	logrus.SetFormatter(safeFormatter{next: formatter})

	return nil
}

// Close 关闭本模块创建的文件输出，供进程退出或重新装配前调用。
// 调用方须先停止日志写入；Lumberjack 在关闭后的下一次写入会重新打开文件。
// 不关闭其他组件注入的 writer，也不承诺等待后台压缩完成。
func Close() error {
	if output, ok := logrus.StandardLogger().Out.(*lumberjack.Logger); ok {
		return output.Close()
	}
	return nil
}
