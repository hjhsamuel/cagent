package config_test

import (
	"github.com/hjhsamuel/cagent/internal/config"
	"reflect"
	"testing"
)

func TestCompressionConfiguration(t *testing.T) {
	values := requiredEnv()
	values["CAGENT_CONTEXT_COMPRESSION_ENABLED"] = "true"
	values["CAGENT_CONTEXT_KEEP_RECENT_ROUNDS"] = "3"
	values["CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT"] = "75"
	c, err := load(values)
	if err != nil || !c.Context.CompressionEnabled || c.Context.KeepRecentRounds != 3 || c.Context.CompressionThresholdPercent != 75 {
		t.Fatal(c.Context, err)
	}
	for _, tc := range []struct{ key, value, field string }{
		{"COMPRESSION_ENABLED", "invalid", "compression_enabled"},
		{"COMPRESSION_ENABLED", "", "compression_enabled"},
		{"KEEP_RECENT_ROUNDS", "0", "keep_recent_rounds"},
		{"KEEP_RECENT_ROUNDS", "-1", "keep_recent_rounds"},
		{"COMPRESSION_THRESHOLD_PERCENT", "0", "compression_threshold_percent"},
		{"COMPRESSION_THRESHOLD_PERCENT", "-1", "compression_threshold_percent"},
		{"COMPRESSION_THRESHOLD_PERCENT", "101", "compression_threshold_percent"},
		{"COMPRESSION_THRESHOLD_PERCENT", "invalid", "compression_threshold_percent"},
	} {
		old := values["CAGENT_CONTEXT_"+tc.key]
		values["CAGENT_CONTEXT_"+tc.key] = tc.value
		got, err := load(values)
		assertInvalid(t, err, "context."+tc.field)
		if !reflect.DeepEqual(got, config.Config{}) {
			t.Fatal("partial config")
		}
		values["CAGENT_CONTEXT_"+tc.key] = old
	}
	values["CAGENT_CONTEXT_COMPRESSION_ENABLED"] = "false"
	values["CAGENT_CONTEXT_KEEP_RECENT_ROUNDS"] = "0"
	values["CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT"] = "0"
	c, err = load(values)
	if err != nil || c.Context.CompressionEnabled {
		t.Fatal("disabled compression rejected", err)
	}
}

func TestRemovedTokenSettingsAreIgnored(t *testing.T) {
	values := requiredEnv()
	for _, name := range []string{"WINDOW_TOKENS", "OUTPUT_TOKENS", "TOOL_TOKENS", "SAFETY_TOKENS", "SUMMARY_WINDOW_TOKENS", "SUMMARY_OUTPUT_TOKENS", "SUMMARY_TOKEN_ENCODING", "COMPRESSION_TRIGGER_TOKENS"} {
		values["CAGENT_CONTEXT_"+name] = "invalid obsolete value"
	}
	got, err := load(values)
	if err != nil || got.Context != config.Defaults().Context {
		t.Fatal("obsolete budget settings were loaded", err)
	}
}
