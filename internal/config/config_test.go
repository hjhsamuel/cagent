package config_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
)

// requiredEnv 使用虚构模型配置，测试只验证配置行为，不声称提供方真实可用。
func requiredEnv() map[string]string {
	return map[string]string{
		"CAGENT_MONGODB_URI": "mongodb://localhost:27017",
	}
}

func load(values map[string]string) (config.Config, error) {
	var names []string
	for name := range values {
		names = append(names, name)
	}
	return config.LoadFromEnv(func(key string) (string, bool) { value, ok := values[key]; return value, ok }, names...)
}

func validConfig(t *testing.T) config.Config {
	t.Helper()
	c, err := load(requiredEnv())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// assertInvalid 验证调用者可以结构化处理错误，不依赖人类说明文本的具体措辞。
func assertInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var detail *apperrors.Error
	if !errors.Is(err, apperrors.ErrInvalidArgument) || !errors.As(err, &detail) {
		t.Fatalf("expected invalid argument, got %v", err)
	}
	if detail.Field() != field || detail.Message() == "" {
		t.Fatalf("expected field %s and safe explanation, got %v", field, err)
	}
}

func TestDefaultsAndRequiredDeploymentSettings(t *testing.T) {
	want := config.Config{
		Capacity:    config.Capacity{Runs: 64, Models: 16, Observations: 32},
		Logging:     config.Logging{Level: "info", Path: "/app/logs/cagent.log", Size: 50, Rolls: 3},
		HTTP:        config.HTTP{Login: config.Login{TokenTTL: time.Hour}, MaxSubscriptions: 256, Address: "127.0.0.1:8080", SSEHeartbeat: 15 * time.Second, ShutdownGrace: 30 * time.Second, WriteTimeout: 10 * time.Second, MaxBodyBytes: 1 << 20},
		MongoDB:     config.MongoDB{Database: "cagent"},
		Agent:       config.Agent{Name: "cagent", MaxTokensField: "max_tokens", RequestTimeout: 2 * time.Minute},
		Tasks:       config.Tasks{PollInterval: 2 * time.Second, ObservationTimeout: 30 * time.Second, ReconnectBackoff: time.Second},
		Maintenance: config.Maintenance{Workers: 32, CancelGrace: 5 * time.Minute, DetachedGrace: 24 * time.Hour, OutageGrace: 30 * time.Minute, InteractionGrace: 24 * time.Hour, MaxBackoff: 5 * time.Minute},
		Context:     config.Context{WindowTokens: 8192, OutputTokens: 2048, ToolTokens: 1024, SafetyTokens: 512, PolicyVersion: "v1", CompressionThresholdPercent: 80, KeepRecentRounds: 2, SummaryWindowTokens: 8192, SummaryOutputTokens: 512},
	}
	if got := config.Defaults(); !reflect.DeepEqual(got, want) {
		t.Fatal("default contract changed")
	}
	assertInvalid(t, want.Validate(), "mongodb.uri")
	got := validConfig(t)
	want.MongoDB.URI = "mongodb://localhost:27017"

	if !reflect.DeepEqual(got, want) {
		t.Fatal("unset environment did not retain defaults")
	}
	// 每次加载都从新的默认值开始，不保存前一次调用的覆盖或凭据。
	got.HTTP.Address = ":9000"
	if next := validConfig(t); !reflect.DeepEqual(next, want) {
		t.Fatal("configuration leaked across loads")
	}
	for key, field := range map[string]string{
		"CAGENT_MONGODB_URI": "mongodb.uri",
	} {
		values := requiredEnv()
		delete(values, key)
		c, err := load(values)
		assertInvalid(t, err, field)
		if !reflect.DeepEqual(c, config.Config{}) {
			t.Fatal("partial configuration returned on failure")
		}
	}
}

func TestEveryEnvironmentOverride(t *testing.T) {
	values := map[string]string{
		"CAGENT_LOG_LEVEL":                 "debug",
		"CAGENT_LOG_PATH":                  "logs/custom.log",
		"CAGENT_LOG_SIZE":                  "10",
		"CAGENT_LOG_ROLL":                  "0",
		"CAGENT_HTTP_ADDRESS":              "[::1]:9090",
		"CAGENT_HTTP_SSE_HEARTBEAT":        "750ms",
		"CAGENT_HTTP_SHUTDOWN_GRACE":       "1m30s",
		"CAGENT_MONGODB_URI":               "mongodb://example.invalid:27017/?replicaSet=example",
		"CAGENT_MONGODB_DATABASE":          "custom",
		"CAGENT_AGENT_NAME":                "assistant",
		"CAGENT_TASKS_POLL_INTERVAL":       "3s",
		"CAGENT_TASKS_OBSERVATION_TIMEOUT": "45s",
		"CAGENT_TASKS_RECONNECT_BACKOFF":   "1.5s",
		"CAGENT_CONTEXT_WINDOW_TOKENS":     "32000",
		"CAGENT_CONTEXT_OUTPUT_TOKENS":     "4000",
		"CAGENT_CONTEXT_TOOL_TOKENS":       "2000",
		"CAGENT_CONTEXT_SAFETY_TOKENS":     "500",
		"CAGENT_CONTEXT_POLICY_VERSION":    "v2",
		"UNRELATED_SETTING":                "ignored",
	}
	got, err := load(values)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		Capacity:    config.Capacity{Runs: 64, Models: 16, Observations: 32},
		Logging:     config.Logging{Level: "debug", Path: "logs/custom.log", Size: 10, Rolls: 0},
		HTTP:        config.HTTP{Login: config.Login{TokenTTL: time.Hour}, MaxSubscriptions: 256, Address: "[::1]:9090", SSEHeartbeat: 750 * time.Millisecond, ShutdownGrace: 90 * time.Second, WriteTimeout: 10 * time.Second, MaxBodyBytes: 1 << 20},
		MongoDB:     config.MongoDB{URI: values["CAGENT_MONGODB_URI"], Database: "custom"},
		Agent:       config.Agent{Name: "assistant", MaxTokensField: "max_tokens", RequestTimeout: 2 * time.Minute},
		Tasks:       config.Tasks{PollInterval: 3 * time.Second, ObservationTimeout: 45 * time.Second, ReconnectBackoff: 1500 * time.Millisecond},
		Maintenance: config.Maintenance{Workers: 32, CancelGrace: 5 * time.Minute, DetachedGrace: 24 * time.Hour, OutageGrace: 30 * time.Minute, InteractionGrace: 24 * time.Hour, MaxBackoff: 5 * time.Minute},
		Context:     config.Context{WindowTokens: 32000, OutputTokens: 4000, ToolTokens: 2000, SafetyTokens: 500, PolicyVersion: "v2", CompressionThresholdPercent: 80, KeepRecentRounds: 2, SummaryWindowTokens: 8192, SummaryOutputTokens: 512},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("environment override did not reach expected field")
	}
	values["CAGENT_CONTEXT_TOOL_TOKENS"], values["CAGENT_CONTEXT_SAFETY_TOKENS"] = "0", "0"
	got, err = load(values)
	if err != nil || got.Context.ToolTokens != 0 || got.Context.SafetyTokens != 0 {
		t.Fatal("explicit zero silently defaulted")
	}
}

func TestExplicitBlankStringsAreNotDefaulted(t *testing.T) {
	for key, field := range map[string]string{
		"CAGENT_HTTP_ADDRESS": "http.address", "CAGENT_MONGODB_URI": "mongodb.uri",
		"CAGENT_MONGODB_DATABASE": "mongodb.database", "CAGENT_AGENT_NAME": "agent.name",

		"CAGENT_CONTEXT_POLICY_VERSION": "context.policy_version",
	} {
		for _, blank := range []string{"", " \t\u3000"} {
			t.Run(key+blank, func(t *testing.T) {
				values := requiredEnv()
				values[key] = blank
				c, err := load(values)
				assertInvalid(t, err, field)
				if !reflect.DeepEqual(c, config.Config{}) {
					t.Fatal("partial configuration returned")
				}
			})
		}
	}
}

func TestInvalidNumbersAndDurations(t *testing.T) {
	for key, field := range map[string]string{
		"CAGENT_HTTP_SSE_HEARTBEAT": "http.sse_heartbeat", "CAGENT_HTTP_SHUTDOWN_GRACE": "http.shutdown_grace",
		"CAGENT_TASKS_POLL_INTERVAL": "tasks.poll_interval", "CAGENT_TASKS_OBSERVATION_TIMEOUT": "tasks.observation_timeout",
		"CAGENT_TASKS_RECONNECT_BACKOFF": "tasks.reconnect_backoff",
	} {
		for _, bad := range []string{"", "30", "invalid", "0", "-1s", " 1s", "999999999999999999999h"} {
			values := requiredEnv()
			values[key] = bad
			c, err := load(values)
			assertInvalid(t, err, field)
			if !reflect.DeepEqual(c, config.Config{}) {
				t.Fatal("partial configuration returned")
			}
		}
	}
	for key, field := range map[string]string{
		"CAGENT_CONTEXT_WINDOW_TOKENS": "context.window_tokens", "CAGENT_CONTEXT_OUTPUT_TOKENS": "context.output_tokens",
		"CAGENT_CONTEXT_TOOL_TOKENS": "context.tool_tokens", "CAGENT_CONTEXT_SAFETY_TOKENS": "context.safety_tokens",
	} {
		for _, bad := range []string{"", "1.5", "0x100", " 100", "NaN", "999999999999999999999999999", "-1"} {
			values := requiredEnv()
			values[key] = bad
			c, err := load(values)
			assertInvalid(t, err, field)
			if !reflect.DeepEqual(c, config.Config{}) {
				t.Fatal("partial configuration returned")
			}
		}
	}
}

func TestAddressValidationWithoutNetworkAccess(t *testing.T) {
	for _, address := range []string{":8080", "127.0.0.1:1", "0.0.0.0:65535", "localhost:8080", "host.example.invalid.:8080", "[::1]:8080", "[fe80::1%eth0]:8080"} {
		c := validConfig(t)
		c.HTTP.Address = address
		if err := c.Validate(); err != nil {
			t.Errorf("valid address rejected: %s: %v", address, err)
		}
	}
	for _, address := range []string{"", "localhost", "http://localhost:8080", "localhost:", ":0", ":65536", ":-1", ":+80", ":http", "::1:8080", "[bad::ip]:8080", "[localhost]:80", "[127.0.0.1]:80", "[]:80", "a b:80", "-bad:80", "a..b:80", "host/path:80"} {
		c := validConfig(t)
		c.HTTP.Address = address
		assertInvalid(t, c.Validate(), "http.address")
	}
}

func TestTokenBudgetBoundariesAndOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name                         string
		window, output, tool, safety int
		field                        string
	}{
		{"one input token", 2, 1, 0, 0, ""},
		{"all reserves", 10, 3, 3, 3, ""},
		{"exactly exhausted", 9, 3, 3, 3, "context.window_tokens"},
		{"over budget", 8, 3, 3, 3, "context.window_tokens"},
		{"zero window", 0, 1, 0, 0, "context.window_tokens"},
		{"zero output", 10, 0, 0, 0, "context.output_tokens"},
		{"negative tool", 10, 1, -1, 0, "context.tool_tokens"},
		{"negative safety", 10, 1, 0, -1, "context.safety_tokens"},
		{"huge valid budget", maxInt, maxInt - 1, 0, 0, ""},
		{"overflowing reserves", maxInt, maxInt - 1, maxInt - 1, maxInt - 1, "context.window_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig(t)
			c.Context = config.Context{WindowTokens: tc.window, OutputTokens: tc.output, ToolTokens: tc.tool, SafetyTokens: tc.safety, PolicyVersion: "v1"}
			before := c
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertInvalid(t, err, tc.field)
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("validation mutated configuration")
			}
		})
	}
}

func TestDirectConfigurationAndSafeErrors(t *testing.T) {
	secret := "private-credential-prompt-mongodb://internal-host"
	for _, tc := range []struct{ key, field string }{
		{"CAGENT_HTTP_ADDRESS", "http.address"},
		{"CAGENT_HTTP_SSE_HEARTBEAT", "http.sse_heartbeat"},
		{"CAGENT_CONTEXT_WINDOW_TOKENS", "context.window_tokens"},
	} {
		values := requiredEnv()
		values[tc.key] = secret
		_, err := load(values)
		assertInvalid(t, err, tc.field)
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			if strings.Contains(fmt.Sprintf("%v %+v %#v", cause, cause, cause), secret) {
				t.Fatal("configuration value leaked through error chain")
			}
		}
	}
	// 直接构造也不能绕过校验；不将合法非空字符串隐式 trim 或展开环境变量。
	c := validConfig(t)
	c.Agent.Model = " model-${UNCHANGED} "
	before := c
	if err := c.Validate(); err != nil || !reflect.DeepEqual(c, before) {
		t.Fatal("validation rewrote opaque string")
	}
	c.Tasks.ObservationTimeout = 0
	assertInvalid(t, c.Validate(), "tasks.observation_timeout")
	c = validConfig(t)
	c.MongoDB.URI = "\u3000"
	assertInvalid(t, c.Validate(), "mongodb.uri")
	zero, err := config.LoadFromEnv(nil)
	assertInvalid(t, err, "environment")
	if !reflect.DeepEqual(zero, config.Config{}) {
		t.Fatal("nil lookup returned usable configuration")
	}
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	// 先通过注入入口列出并固定所有已知键，避免开发机残留环境影响此测试。
	// 不并行运行：t.Setenv 修改进程环境，结束后由 testing 恢复。
	_, _ = config.LoadFromEnv(func(key string) (string, bool) {
		t.Setenv(key, "")
		return "", false
	})
	for key, value := range map[string]string{
		"CAGENT_HTTP_LOGIN_TOKEN_TTL": "1h",
		"CAGENT_CAPACITY_RUNS":        "64", "CAGENT_CAPACITY_MODELS": "16", "CAGENT_CAPACITY_OBSERVATIONS": "32", "CAGENT_HTTP_MAX_SUBSCRIPTIONS": "256",
		"CAGENT_LOG_LEVEL": "info", "CAGENT_LOG_PATH": "logs/test.log", "CAGENT_LOG_SIZE": "1", "CAGENT_LOG_ROLL": "2",
		"CAGENT_HTTP_WRITE_TIMEOUT": "10s", "CAGENT_HTTP_MAX_BODY_BYTES": "1048576", "CAGENT_HTTP_ADDRESS": ":9191", "CAGENT_HTTP_SSE_HEARTBEAT": "10s", "CAGENT_HTTP_SHUTDOWN_GRACE": "20s",
		"CAGENT_MONGODB_URI": "mongodb://localhost:27017", "CAGENT_MONGODB_DATABASE": "test",
		"CAGENT_AGENT_REQUEST_TIMEOUT": "2m", "CAGENT_AGENT_NAME": "test", "CAGENT_AGENT_PROVIDER": "test",
		"CAGENT_TASKS_POLL_INTERVAL": "1s", "CAGENT_TASKS_OBSERVATION_TIMEOUT": "5s", "CAGENT_TASKS_RECONNECT_BACKOFF": "2s",
		"CAGENT_MAINTENANCE_WORKERS": "32", "CAGENT_MAINTENANCE_CANCEL_GRACE": "5m", "CAGENT_MAINTENANCE_DETACHED_GRACE": "24h", "CAGENT_MAINTENANCE_OUTAGE_GRACE": "30m", "CAGENT_MAINTENANCE_INTERACTION_GRACE": "24h", "CAGENT_MAINTENANCE_MAX_BACKOFF": "5m",
		"CAGENT_CONTEXT_WINDOW_TOKENS": "100", "CAGENT_CONTEXT_OUTPUT_TOKENS": "10", "CAGENT_CONTEXT_TOOL_TOKENS": "0",
		"CAGENT_CONTEXT_SAFETY_TOKENS": "1", "CAGENT_CONTEXT_POLICY_VERSION": "test",
		"CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT": "80", "CAGENT_CONTEXT_KEEP_RECENT_ROUNDS": "2",
		"CAGENT_CONTEXT_ARCHIVE_COMPLETED":     "false",
		"CAGENT_CONTEXT_SUMMARY_WINDOW_TOKENS": "8192", "CAGENT_CONTEXT_SUMMARY_OUTPUT_TOKENS": "512",
	} {
		t.Setenv(key, value)
	}
	c, err := config.Load()
	if err != nil || c.HTTP.Address != ":9191" || c.Context.WindowTokens != 100 {
		t.Fatalf("process environment not loaded: %v", err)
	}
}

func TestDocumentedExampleLoads(t *testing.T) {
	// 此处按 key=value 清单验证配置键；dotenv 语法由命令入口的 godotenv 解析。
	file, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatal("invalid example assignment")
		}
		if _, duplicate := values[key]; duplicate {
			t.Fatal("duplicate example key")
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range values {
		names = append(names, name)
	}
	_, err = config.LoadFromEnv(func(key string) (string, bool) {
		value, ok := values[key]
		if !ok {
			t.Errorf("example missing key %s", key)
		}
		delete(values, key)
		return value, ok
	}, names...)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatal("example contains unknown settings")
	}
}
