package config

import (
	"strings"
	"time"
)

// Tools 控制启动时本地目录扫描和所有工具的调用限制。远端连接来自 MongoDB。
type Tools struct {
	LocalDir       string
	Timeout        time.Duration
	MaxInputBytes  int
	MaxOutputBytes int
	MaxModelCalls  int
}

func DefaultTools() Tools {
	return Tools{LocalDir: "local-tools", Timeout: 30 * time.Second, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxModelCalls: 16}
}

func (t Tools) Validate() error {
	if strings.TrimSpace(t.LocalDir) == "" {
		return invalid("tools.local_dir", "must not be blank")
	}
	if t.Timeout <= 0 {
		return invalid("tools.timeout", "must be greater than zero")
	}
	if t.MaxInputBytes < 1 || t.MaxInputBytes > 4<<20 {
		return invalid("tools.max_input_bytes", "must be between 1 byte and 4 MiB")
	}
	if t.MaxOutputBytes < 256 || t.MaxOutputBytes > 8<<20 {
		return invalid("tools.max_output_bytes", "must be between 256 bytes and 8 MiB")
	}
	if t.MaxModelCalls < 1 || t.MaxModelCalls > 128 {
		return invalid("tools.max_model_calls", "must be between 1 and 128")
	}
	return nil
}
