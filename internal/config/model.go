package config

import (
	"net/url"
	"strings"
)

// ValidateOpenAI 校验用户显式指定的兼容端，不测试网络，不设置模型/地址/密钥默认值。
// 允许本地 HTTP 测试和私有兼容端；不接受 URL 内凭据、查询密钥或片段，避免泄露。
func (a Agent) ValidateOpenAI() error {
	for _, v := range []struct{ field, value string }{{"agent.model", a.Model}, {"agent.api_key", a.APIKey}, {"agent.base_url", a.BaseURL}} {
		if strings.TrimSpace(v.value) == "" {
			return invalid(v.field, "must not be blank")
		}
	}
	u, e := url.Parse(a.BaseURL)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("agent.base_url", "must be an HTTP API base URL without credentials, query or fragment")
	}
	if a.TokenEncoding != "cl100k_base" && a.TokenEncoding != "o200k_base" {
		return invalid("agent.token_encoding", "must explicitly match a supported tokenizer encoding")
	}
	if a.MaxTokensField != "max_tokens" && a.MaxTokensField != "max_completion_tokens" {
		return invalid("agent.max_tokens_field", "unsupported output limit field")
	}
	if a.RequestTimeout <= 0 {
		return invalid("agent.request_timeout", "must be positive")
	}
	return nil
}
