package config

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// Logging 控制 Logrus 默认实例及 Lumberjack 文件轮转；输出固定为文本格式。
type Logging struct {
	// Level 接受 trace/debug/info/warn/error 及 Logrus 的大小写、warning 别名。
	// 不接受 fatal/panic 阈值，避免屏蔽普通错误和生命周期日志。
	Level string
	// Path 是日志文件路径，不允许空白、NUL 或目录形式；不会隐式 trim。
	Path string
	// Size 是文件大小上限，单位 MiB，必须为正且换算为 int64 字节数不能溢出。
	Size int
	// Rolls 是备份文件保留数量，必须非负；0 显式表示不限制数量。
	Rolls int
}

// Validate 独立验证日志选项，允许在数据库/模型配置尚未加载前建立启动日志。
// 不访问文件系统；路径权限、磁盘空间等在实际写入时由 Lumberjack 检查。
func (c Logging) Validate() error {
	level, err := logrus.ParseLevel(c.Level)
	if err != nil || level < logrus.ErrorLevel {
		return invalid("logging.level", "must be trace, debug, info, warn or error")
	}
	if strings.TrimSpace(c.Path) == "" || strings.ContainsRune(c.Path, 0) || strings.HasSuffix(c.Path, "/") || strings.HasSuffix(c.Path, "\\") || filepath.Base(c.Path) == "." || filepath.Base(c.Path) == ".." {
		return invalid("logging.path", "must be a nonblank file path")
	}
	const maxSizeMiB = int64(1<<63-1) / (1024 * 1024)
	if c.Size <= 0 || int64(c.Size) > maxSizeMiB {
		return invalid("logging.size", "must be positive and fit in an int64 byte count when converted from MiB")
	}
	if c.Rolls < 0 {
		return invalid("logging.rolls", "must not be negative; zero retains all backups")
	}

	return nil
}

// LoadLoggingFromEnv 只读取四个日志键，采用与完整配置相同的覆盖与校验规则。
// 启动入口可先调用它记录后续装配错误，不要求先提供 MongoDB 或模型配置。
// lookup 应提供本次调用内稳定的值；显式空值不能回退，失败返回零值 Logging。
func LoadLoggingFromEnv(lookup func(string) (string, bool)) (Logging, error) {
	if lookup == nil {
		return Logging{}, invalid("environment", "environment lookup is required")
	}
	c := Defaults().Logging
	if value, ok := lookup("CAGENT_LOG_LEVEL"); ok {
		c.Level = value
	}
	if value, ok := lookup("CAGENT_LOG_PATH"); ok {
		c.Path = value
	}
	if value, ok := lookup("CAGENT_LOG_SIZE"); ok {
		if maxSize, err := strconv.Atoi(value); err != nil {
			return Logging{}, invalid("logging.size", "must be an integer")
		} else {
			c.Size = maxSize
		}
	}
	if value, ok := lookup("CAGENT_LOG_ROLL"); ok {
		if rolls, err := strconv.Atoi(value); err != nil {
			return Logging{}, invalid("logging.rolls", "must be an integer")
		} else {
			c.Rolls = rolls
		}
	}
	if err := c.Validate(); err != nil {
		return Logging{}, err
	}
	return c, nil
}
