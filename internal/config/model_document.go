package config

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

// Thinking 定义开启深度思考的 JSON 参数路径与值，例如 thinking.type=enabled。
type Thinking struct {
	Enabled bool   `json:"enabled"`
	Key     string `json:"key"`
	Value   any    `json:"value"`
}

func (t Thinking) Validate() error {
	if !t.Enabled {
		return nil
	}
	parts := strings.Split(t.Key, ".")
	for _, part := range parts {
		if part == "" {
			return invalid("model.thinking.key", "must be a dotted JSON parameter path")
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
				return invalid("model.thinking.key", "invalid JSON parameter path")
			}
		}
	}
	for _, reserved := range []string{"model", "messages", "tools", "tool_choice", "stream", "stream_options", "max_tokens", "max_completion_tokens", "api_key"} {
		if parts[0] == reserved {
			return invalid("model.thinking.key", "cannot override a reserved request parameter")
		}
	}
	if t.Value == nil {
		return invalid("model.thinking.value", "must specify a JSON value")
	}
	if _, err := json.Marshal(t.Value); err != nil {
		return invalid("model.thinking.value", "must be a JSON value")
	}
	return nil
}

// ResolveModel 将集中维护的 MongoDB 文档转换为已校验、已解密的运行时配置。
func ResolveModel(d schema.Model, name string, ring *Keyring) (Agent, error) {
	if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Provider) == "" {
		return Agent{}, invalid("model", "model id and provider must not be blank")
	}
	timeout, err := time.ParseDuration(d.Options.RequestTimeout)
	if err != nil {
		return Agent{}, invalid("model.config.request_timeout", "must be a positive duration")
	}
	if d.Options.WindowTokens <= 0 || d.Options.OutputTokens <= 0 || d.Options.OutputTokens > math.MaxInt32 || d.Options.OutputTokens >= d.Options.WindowTokens {
		return Agent{}, invalid("model.config.window_tokens", "must exceed a positive output limit within int32 range")
	}
	a := Agent{Name: name, Provider: d.Provider, Model: d.Model, BaseURL: d.BaseURL, TokenEncoding: d.Options.TokenEncoding, MaxTokensField: d.Options.MaxTokensField, RequestTimeout: timeout, Thinking: &Thinking{Enabled: d.Options.Thinking.Enabled, Key: d.Options.Thinking.Key, Value: d.Options.Thinking.Value}, APIKey: "validation-only"}
	if err := a.ValidateOpenAI(); err != nil {
		return Agent{}, err
	}
	a.APIKey = ""
	a.Keys, err = NewKeyPool(d.APIKeys, ring)
	if err != nil {
		return Agent{}, err
	}
	return a, nil
}
