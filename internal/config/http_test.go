package config_test

import (
	"github.com/hjhsamuel/cagent/internal/config"
	"strings"
	"testing"
	"time"
)

func TestHTTPServerSettings(t *testing.T) {
	values := requiredEnv()
	for k, v := range map[string]string{"CAGENT_HTTP_JWT_SECRET": strings.Repeat("x", 32), "CAGENT_HTTP_WRITE_TIMEOUT": "3s", "CAGENT_HTTP_MAX_BODY_BYTES": "4096"} {
		values[k] = v
	}
	cfg, err := load(values)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.JWT.Secret != values["CAGENT_HTTP_JWT_SECRET"] || cfg.HTTP.WriteTimeout != 3*time.Second || cfg.HTTP.MaxBodyBytes != 4096 || cfg.HTTP.ValidateServer() != nil {
		t.Fatal("server config not loaded")
	}
	for _, mutate := range []func(*config.HTTP){func(h *config.HTTP) { h.JWT.Secret = "short" }, func(h *config.HTTP) { h.JWT.Secret = "" }, func(h *config.HTTP) { h.JWT.Secret = strings.Repeat(" ", 32) }, func(h *config.HTTP) { h.MaxBodyBytes = 0 }, func(h *config.HTTP) { h.WriteTimeout = 0 }} {
		h := cfg.HTTP
		mutate(&h)
		err := h.ValidateServer()
		if err == nil || strings.Contains(err.Error(), cfg.HTTP.JWT.Secret) {
			t.Fatal("invalid settings accepted or secret leaked")
		}
	}
}
