package config_test

import (
	"github.com/hjhsamuel/cagent/internal/config"
	"testing"
)

// 所有策略选项均经过环境加载；禁用、边界与安全错误不能依赖后续模型调用发现。
func TestCompressionConfiguration(t *testing.T) {
	v := requiredEnv()
	v["CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT"] = "65"
	v["CAGENT_CONTEXT_KEEP_RECENT_ROUNDS"] = "3"
	v["CAGENT_CONTEXT_SUMMARY_MODEL"] = "summary-model"
	v["CAGENT_CONTEXT_SUMMARY_TOKEN_ENCODING"] = "cl100k_base"
	v["CAGENT_CONTEXT_SUMMARY_WINDOW_TOKENS"] = "4000"
	v["CAGENT_CONTEXT_SUMMARY_OUTPUT_TOKENS"] = "400"
	c, e := load(v)
	if e != nil || c.Context.CompressionThresholdPercent != 65 || c.Context.KeepRecentRounds != 3 || c.Context.SummaryModel != "summary-model" || c.Context.SummaryTokenEncoding != "cl100k_base" || c.Context.SummaryWindowTokens != 4000 || c.Context.SummaryOutputTokens != 400 {
		t.Fatal(c.Context, e)
	}
	for _, tt := range []struct{ key, value, field string }{
		{"COMPRESSION_THRESHOLD_PERCENT", "101", "compression_threshold_percent"},
		{"COMPRESSION_THRESHOLD_PERCENT", "-1", "compression_threshold_percent"},
		{"COMPRESSION_THRESHOLD_PERCENT", "", "compression_threshold_percent"},
		{"KEEP_RECENT_ROUNDS", "0", "keep_recent_rounds"},
		{"SUMMARY_MODEL", " ", "summary_model"},
		{"SUMMARY_TOKEN_ENCODING", "wrong", "summary_token_encoding"},
		{"SUMMARY_WINDOW_TOKENS", "900", "summary_window_tokens"},
		{"SUMMARY_OUTPUT_TOKENS", "0", "summary_window_tokens"},
	} {
		old := v["CAGENT_CONTEXT_"+tt.key]
		v["CAGENT_CONTEXT_"+tt.key] = tt.value
		got, err := load(v)
		assertInvalid(t, err, "context."+tt.field)
		if got != (config.Config{}) {
			t.Fatal("partial config")
		}
		v["CAGENT_CONTEXT_"+tt.key] = old
	}
	if e := (config.Context{}).ValidateCompression(); e != nil {
		t.Fatal("disabled rejected", e)
	}
}
