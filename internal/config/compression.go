package config

import (
	"math"
	"strings"
)

// ValidateCompression 同时用于环境加载和程序化装配；禁用时允许其余选项为零，
// 但负阈值不是禁用。摘要输出受 SDK int32 字段约束，窗口额外预留主配置安全余量。
func (c Context) ValidateCompression() error {
	if c.CompressionThresholdPercent < 0 || c.CompressionThresholdPercent > 100 {
		return invalid("context.compression_threshold_percent", "must be between 0 and 100")
	}
	if c.CompressionThresholdPercent == 0 {
		return nil
	}
	if c.KeepRecentRounds < 1 {
		return invalid("context.keep_recent_rounds", "must be positive")
	}
	if c.SummaryOutputTokens <= 0 || c.SummaryOutputTokens > math.MaxInt32 || c.SummaryWindowTokens <= c.SummaryOutputTokens || c.SafetyTokens < 0 || c.SummaryWindowTokens-c.SummaryOutputTokens <= c.SafetyTokens {
		return invalid("context.summary_window_tokens", "must leave positive summary input budget")
	}
	if c.SummaryModel != "" && strings.TrimSpace(c.SummaryModel) == "" {
		return invalid("context.summary_model", "must not be blank")
	}
	if c.SummaryTokenEncoding != "" && c.SummaryTokenEncoding != "cl100k_base" && c.SummaryTokenEncoding != "o200k_base" {
		return invalid("context.summary_token_encoding", "unsupported tokenizer encoding")
	}
	return nil
}
