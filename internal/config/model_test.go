package config_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/config"
)

func openAIEnv() map[string]string {
	v := requiredEnv()
	v["CAGENT_AGENT_PROVIDER"] = "openai"
	v["CAGENT_AGENT_MODEL"] = "vendor/arbitrary-model"
	v["CAGENT_AGENT_BASE_URL"] = "https://example.invalid/custom/v1"
	v["CAGENT_AGENT_API_KEY"] = "unit-secret"
	v["CAGENT_AGENT_TOKEN_ENCODING"] = "cl100k_base"
	v["CAGENT_AGENT_MAX_TOKENS_FIELD"] = "max_completion_tokens"
	v["CAGENT_AGENT_REQUEST_TIMEOUT"] = "45s"
	return v
}

// 动态参数逐次加载，不以固定模型名称推断 URL/密钥/编码，也不把密钥写入错误。
func TestOpenAIDynamicConfiguration(t *testing.T) {
	v := openAIEnv()
	c, e := load(v)
	if e != nil {
		t.Fatal(e)
	}
	if c.Agent.Model != v["CAGENT_AGENT_MODEL"] || c.Agent.BaseURL != v["CAGENT_AGENT_BASE_URL"] || c.Agent.APIKey != v["CAGENT_AGENT_API_KEY"] || c.Agent.TokenEncoding != "cl100k_base" || c.Agent.MaxTokensField != "max_completion_tokens" || c.Agent.RequestTimeout != 45*time.Second {
		t.Fatal("configuration mismatch")
	}
	v["CAGENT_AGENT_MODEL"] = "another-model"
	v["CAGENT_AGENT_API_KEY"] = "other-secret"
	second, e := load(v)
	if e != nil || second.Agent.Model == c.Agent.Model || second.Agent.APIKey == c.Agent.APIKey {
		t.Fatal("stale configuration")
	}
	for _, tt := range []struct{ key, value, field string }{
		{"CAGENT_AGENT_MODEL", "", "agent.model"}, {"CAGENT_AGENT_BASE_URL", "https://user:unit-secret@example.invalid", "agent.base_url"}, {"CAGENT_AGENT_BASE_URL", "https://example.invalid/?key=unit-secret", "agent.base_url"}, {"CAGENT_AGENT_BASE_URL", "file:///tmp/test", "agent.base_url"},
		{"CAGENT_AGENT_API_KEY", " ", "agent.api_key"}, {"CAGENT_AGENT_TOKEN_ENCODING", "unknown", "agent.token_encoding"}, {"CAGENT_AGENT_MAX_TOKENS_FIELD", "unknown", "agent.max_tokens_field"}, {"CAGENT_AGENT_REQUEST_TIMEOUT", "0s", "agent.request_timeout"},
	} {
		t.Run(tt.field+tt.value, func(t *testing.T) {
			v := openAIEnv()
			v[tt.key] = tt.value
			c, e := load(v)
			assertInvalid(t, e, tt.field)
			if c != (config.Config{}) {
				t.Fatal("partial config")
			}
			if strings.Contains(fmt.Sprintf("%+v", e), "unit-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}
