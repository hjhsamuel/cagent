package config_test

import (
	"github.com/hjhsamuel/cagent/internal/config"
	"testing"
)

func TestCapacityEnvironmentAndBounds(t *testing.T) {
	for key, field := range map[string]string{"CAGENT_CAPACITY_RUNS": "capacity.runs", "CAGENT_CAPACITY_MODELS": "capacity.models", "CAGENT_CAPACITY_OBSERVATIONS": "capacity.observations", "CAGENT_HTTP_MAX_SUBSCRIPTIONS": "http.max_subscriptions"} {
		for _, value := range []string{"", "0", "-1", "100001", "secret"} {
			env := requiredEnv()
			env[key] = value
			_, err := load(env)
			assertInvalid(t, err, field)
		}
	}
	env := requiredEnv()
	env["CAGENT_CAPACITY_RUNS"] = "3"
	env["CAGENT_CAPACITY_MODELS"] = "2"
	env["CAGENT_CAPACITY_OBSERVATIONS"] = "1"
	env["CAGENT_HTTP_MAX_SUBSCRIPTIONS"] = "4"
	c, err := load(env)
	if err != nil || c.Capacity != (config.Capacity{Runs: 3, Models: 2, Observations: 1}) || c.HTTP.MaxSubscriptions != 4 {
		t.Fatal(c.Capacity, err)
	}
}
