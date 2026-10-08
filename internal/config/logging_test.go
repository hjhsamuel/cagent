package config_test

import (
	"github.com/hjhsamuel/cagent/internal/config"
	"strconv"
	"testing"
)

func TestLoggingConfiguration(t *testing.T) {
	for _, level := range []string{"trace", "debug", "info", "warn", "error", "INFO", "warning"} {
		values := requiredEnv()
		values["CAGENT_LOG_LEVEL"] = level
		values["CAGENT_LOG_PATH"] = "logs/custom.log"
		values["CAGENT_LOG_SIZE"] = "12"
		values["CAGENT_LOG_ROLL"] = "0"
		c, err := load(values)
		want := config.Logging{Level: level, Path: "logs/custom.log", Size: 12, Rolls: 0}
		if err != nil || c.Logging != want {
			t.Fatalf("logging override failed: %v", err)
		}
	}
	for _, tc := range []struct{ key, value, field string }{
		{"CAGENT_LOG_LEVEL", "", "logging.level"},
		{"CAGENT_LOG_LEVEL", " info", "logging.level"},
		{"CAGENT_LOG_LEVEL", "fatal", "logging.level"},
		{"CAGENT_LOG_LEVEL", "panic", "logging.level"},
		{"CAGENT_LOG_LEVEL", "unknown", "logging.level"},
		{"CAGENT_LOG_PATH", "", "logging.path"},
		{"CAGENT_LOG_PATH", " \t\u3000", "logging.path"},
		{"CAGENT_LOG_PATH", "logs/", "logging.path"},
		{"CAGENT_LOG_PATH", "logs\\", "logging.path"},
		{"CAGENT_LOG_PATH", ".", "logging.path"},
		{"CAGENT_LOG_PATH", "..", "logging.path"},
		{"CAGENT_LOG_PATH", "bad\x00.log", "logging.path"},
		{"CAGENT_LOG_SIZE", "", "logging.size"},
		{"CAGENT_LOG_SIZE", "0", "logging.size"},
		{"CAGENT_LOG_SIZE", "-1", "logging.size"},
		{"CAGENT_LOG_SIZE", "1.5", "logging.size"},
		{"CAGENT_LOG_SIZE", "999999999999999999999999", "logging.size"},
		{"CAGENT_LOG_ROLL", "", "logging.rolls"},
		{"CAGENT_LOG_ROLL", "-1", "logging.rolls"},
		{"CAGENT_LOG_ROLL", "many", "logging.rolls"},
		{"CAGENT_LOG_ROLL", "999999999999999999999999", "logging.rolls"},
	} {
		t.Run(tc.key+"/"+strconv.Quote(tc.value), func(t *testing.T) {
			values := requiredEnv()
			values[tc.key] = tc.value
			c, err := load(values)
			assertInvalid(t, err, tc.field)
			if c != (config.Config{}) {
				t.Fatal("partial configuration returned")
			}
		})
	}
	options, err := config.LoadLoggingFromEnv(func(string) (string, bool) { return "", false })
	if err != nil || options != config.Defaults().Logging {
		t.Fatal("logging requires business settings")
	}
	_, err = config.LoadLoggingFromEnv(nil)
	assertInvalid(t, err, "environment")
}

// 直接构造也必须校验；超大 MiB 不能在转换字节数时溢出。
func TestLoggingDirectValidation(t *testing.T) {
	for _, tc := range []struct {
		mutate func(*config.Logging)
		field  string
	}{
		{func(c *config.Logging) { c.Path = "" }, "logging.path"},
		{func(c *config.Logging) { c.Size = 0 }, "logging.size"},
		{func(c *config.Logging) { c.Rolls = -1 }, "logging.rolls"},
	} {
		c := config.Defaults().Logging
		tc.mutate(&c)
		assertInvalid(t, c.Validate(), tc.field)
	}
	if strconv.IntSize == 64 {
		maxMiB := int64(1<<63-1) / (1024 * 1024)
		c := config.Defaults().Logging
		c.Size = int(maxMiB)
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		c.Size++
		assertInvalid(t, c.Validate(), "logging.size")
	}
}
