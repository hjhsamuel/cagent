package config

import (
	"net/url"
	"strings"
)

// ValidateOpenAI 校验用户显式指定的兼容端，不测试网络，不设置模型/地址/密钥默认值。
// 允许本地 HTTP 测试和私有兼容端；不接受 URL 内凭据、查询密钥或片段，避免泄露。
func (a Agent) ValidateOpenAI() error {
	for _, v := range []struct{ field, value string }{{"agent.model", a.Model}, {"agent.base_url", a.BaseURL}} {
		if strings.TrimSpace(v.value) == "" {
			return invalid(v.field, "must not be blank")
		}
	}
	if a.Keys == nil && strings.TrimSpace(a.APIKey) == "" {
		return invalid("agent.api_key", "must not be blank")
	}
	if a.Keys != nil && a.Keys.total <= 0 {
		return invalid("agent.api_key", "must have an initialized weighted key pool")
	}
	if a.Thinking != nil {
		if err := a.Thinking.Validate(); err != nil {
			return err
		}
	}
	u, e := url.Parse(a.BaseURL)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("agent.base_url", "must be an HTTP API base URL without credentials, query or fragment")
	}
	if a.RequestTimeout <= 0 {
		return invalid("agent.request_timeout", "must be positive")
	}
	return nil
}
