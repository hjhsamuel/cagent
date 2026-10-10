package config_test

import (
	"testing"
	"time"
)

func TestToolLoadingEnvironmentAndLimits(t *testing.T) {
	values := requiredEnv()
	values["CAGENT_LOCAL_TOOLS_DIR"] = "custom-tools"
	values["CAGENT_TOOLS_TIMEOUT"] = "5s"
	values["CAGENT_TOOLS_MAX_INPUT_BYTES"] = "2048"
	values["CAGENT_TOOLS_MAX_OUTPUT_BYTES"] = "4096"
	values["CAGENT_TOOLS_MAX_MODEL_CALLS"] = "8"
	cfg, err := load(values)
	if err != nil || cfg.Tools.LocalDir != "custom-tools" || cfg.Tools.Timeout != 5*time.Second || cfg.Tools.MaxInputBytes != 2048 || cfg.Tools.MaxOutputBytes != 4096 || cfg.Tools.MaxModelCalls != 8 {
		t.Fatal("tool settings not loaded", err)
	}
	for _, test := range []struct{ key, value, field string }{
		{"CAGENT_LOCAL_TOOLS_DIR", "", "tools.local_dir"},
		{"CAGENT_TOOLS_TIMEOUT", "0s", "tools.timeout"},
		{"CAGENT_TOOLS_TIMEOUT", "bad", "tools.timeout"},
		{"CAGENT_TOOLS_MAX_INPUT_BYTES", "4194305", "tools.max_input_bytes"},
		{"CAGENT_TOOLS_MAX_OUTPUT_BYTES", "255", "tools.max_output_bytes"},
		{"CAGENT_TOOLS_MAX_MODEL_CALLS", "129", "tools.max_model_calls"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			bad := requiredEnv()
			bad[test.key] = test.value
			_, err := load(bad)
			assertInvalid(t, err, test.field)
		})
	}
}
