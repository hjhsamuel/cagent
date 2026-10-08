package config_test

import (
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/config"
)

func TestLoginConfiguration(t *testing.T) {
	values := requiredEnv()
	values["CAGENT_HTTP_LOGIN_TOKEN_TTL"] = "30m"
	// Obsolete account settings are ignored, including invalid password hashes.
	values["CAGENT_HTTP_LOGIN_USERNAME"] = "unused"
	values["CAGENT_HTTP_LOGIN_PASSWORD_HASH"] = "unused"
	cfg, err := load(values)
	if err != nil || cfg.HTTP.Login.TokenTTL != 30*time.Minute {
		t.Fatal("login TTL not loaded", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, 25 * time.Hour, 1500 * time.Millisecond} {
		if err := (config.Login{TokenTTL: ttl}).Validate(); err == nil {
			t.Fatal("invalid TTL accepted", ttl)
		}
	}
	for _, ttl := range []time.Duration{time.Second, time.Hour, 24 * time.Hour} {
		if err := (config.Login{TokenTTL: ttl}).Validate(); err != nil {
			t.Fatal("valid TTL rejected", err)
		}
	}
}
