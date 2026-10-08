package config_test

import (
	"fmt"
	"github.com/hjhsamuel/cagent/internal/config"
	"strings"
	"testing"
	"time"
)

func TestModelEnvironmentDoesNotSelectDefaultModel(t *testing.T) {
	v := requiredEnv()
	for _, key := range []string{"CAGENT_AGENT_MODEL", "CAGENT_AGENT_PROVIDER", "CAGENT_AGENT_BASE_URL", "CAGENT_AGENT_API_KEY", "CAGENT_AGENT_TOKEN_ENCODING", "CAGENT_AGENT_MAX_TOKENS_FIELD", "CAGENT_AGENT_REQUEST_TIMEOUT"} {
		v[key] = "obsolete-secret-value"
	}
	v["CAGENT_MODEL_ENCRYPTION_KEY_V1"] = "environment-only-secret"
	c, err := load(v)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.Model != "" || c.Agent.Provider != "" || c.Agent.BaseURL != "" || c.Agent.APIKey != "" || c.Agent.TokenEncoding != "" || c.Agent.MaxTokensField != "max_tokens" || c.Agent.RequestTimeout != 2*time.Minute {
		t.Fatal("obsolete model environment settings were loaded")
	}
	if c.ModelEncryption.Keys["v1"] != v["CAGENT_MODEL_ENCRYPTION_KEY_V1"] {
		t.Fatal("keyring environment not loaded")
	}
	if strings.Contains(fmt.Sprintf("%+v", c.ModelEncryption), "environment-only-secret") {
		t.Fatal("encryption config leaked")
	}
}

func TestOpenAIProgrammaticValidation(t *testing.T) {
	valid := config.Agent{Provider: "GLM", Model: "arbitrary-model", BaseURL: "https://example.invalid/v1", APIKey: "unit-secret", TokenEncoding: "cl100k_base", MaxTokensField: "max_tokens", RequestTimeout: time.Minute}
	if err := valid.ValidateOpenAI(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field  string
		change func(*config.Agent)
	}{
		{"agent.model", func(a *config.Agent) { a.Model = "" }},
		{"agent.base_url", func(a *config.Agent) { a.BaseURL = "https://user:unit-secret@example.invalid" }},
		{"agent.base_url", func(a *config.Agent) { a.BaseURL = "https://example.invalid/?key=unit-secret" }},
		{"agent.api_key", func(a *config.Agent) { a.APIKey = " " }},
		{"agent.token_encoding", func(a *config.Agent) { a.TokenEncoding = "unknown" }},
		{"agent.max_tokens_field", func(a *config.Agent) { a.MaxTokensField = "unknown" }},
		{"agent.request_timeout", func(a *config.Agent) { a.RequestTimeout = 0 }},
	} {
		a := valid
		tc.change(&a)
		err := a.ValidateOpenAI()
		assertInvalid(t, err, tc.field)
		if strings.Contains(fmt.Sprintf("%+v", err), "unit-secret") {
			t.Fatal("secret leaked")
		}
	}
}
