package config

import (
	"strings"
)

// ValidateCompression 同时用于环境加载和程序化装配；开启时必须保留至少一个近期轮次。
func (c Context) ValidateCompression() error {
	if !c.CompressionEnabled {
		return nil
	}
	if c.CompressionThresholdPercent < 1 || c.CompressionThresholdPercent > 100 {
		return invalid("context.compression_threshold_percent", "must be between 1 and 100")
	}
	if c.KeepRecentRounds < 1 {
		return invalid("context.keep_recent_rounds", "must be positive")
	}
	if c.SummaryModel != "" && strings.TrimSpace(c.SummaryModel) == "" {
		return invalid("context.summary_model", "must not be blank")
	}
	return nil
}
