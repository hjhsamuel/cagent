package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func adminInput() ModelInput {
	secret := "supplier-secret"
	return ModelInput{Model: "vendor-model", Provider: "GLM", BaseURL: "https://example.invalid/v1", APIKeys: []ModelKeyInput{{ID: "key-1", Value: &secret, Weight: 1}}, Options: schema.ModelConfig{WindowTokens: 32768, RequestTimeout: "2s", Thinking: schema.Thinking{Enabled: true, Key: "thinking", Value: map[string]any{"type": "enabled"}}}}
}

func TestModelAdministrationPersistenceAndBindings(t *testing.T) {
	c, err := NewModelCatalog(nil, "agent", NewDeferredKeyring(ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}}))
	if err != nil {
		t.Fatal(err)
	}
	var stored schema.Model
	writes := 0
	persist := func(old, updated *schema.Model) error {
		writes++
		if writes == 1 && old != nil {
			t.Fatal("create has old document")
		}
		if updated != nil {
			stored = *updated
		}
		return nil
	}
	input := adminInput()
	view, err := c.PutModelConfiguration("model", input, nil, persist)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "supplier-secret") || strings.Contains(string(data), "ciphertext") || strings.Contains(string(data), "nonce") {
		t.Fatal("credential exposed")
	}
	if stored.APIKeys[0].Ciphertext == "" || stored.APIKeys[0].Version != "v1" {
		t.Fatal("key not encrypted")
	}
	// Neither returned views nor input maps may mutate the published snapshot.
	view.Options.Thinking.Value.(map[string]any)["type"] = "mutated"
	input.Options.Thinking.Value.(map[string]any)["type"] = "mutated"
	selected, err := c.Bind("model", "key-1")
	if err != nil || selected.Agent.APIKey != "supplier-secret" || selected.Options.Thinking.Value.(map[string]any)["type"] != "enabled" {
		t.Fatal("binding or snapshot lost", err)
	}
	input = adminInput()
	input.APIKeys[0].Value = nil
	input.Model = "updated-vendor"
	if _, err := c.PutModelConfiguration("model", input, nil, persist); err != nil {
		t.Fatal(err)
	}
	selected, err = c.Bind("model", "key-1")
	if err != nil || selected.Agent.Model != input.Model || selected.Agent.APIKey != "supplier-secret" {
		t.Fatal("update did not preserve key", err)
	}
	before := stored.APIKeys[0]
	if _, err := c.RotateKeys(func(ModelEncryption) error { return nil }, func(old, updated []schema.Model) error {
		stored = updated[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stored.APIKeys[0].Version != "v2" || stored.APIKeys[0].ID != before.ID {
		t.Fatal("rotation lost binding")
	}
	if _, err := c.PutModelConfiguration("model", input, nil, persist); err != nil {
		t.Fatal(err)
	}
	if stored.APIKeys[0].Version != "v2" {
		t.Fatal("update reverted rotation")
	}
	fail := func(*schema.Model, *schema.Model) error { return apperrors.ErrConflict }
	input.Model = "should-not-publish"
	if _, err := c.PutModelConfiguration("model", input, nil, fail); !errors.Is(err, apperrors.ErrConflict) {
		t.Fatal(err)
	}
	if err := c.DeleteModelConfiguration("model", fail); !errors.Is(err, apperrors.ErrConflict) {
		t.Fatal(err)
	}
	selected, err = c.Bind("model", "key-1")
	if err != nil || selected.Agent.Model != "updated-vendor" {
		t.Fatal("failed persistence changed catalog", err)
	}
	if err := c.DeleteModelConfiguration("model", persist); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind("model", "key-1"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("deleted model still bound", err)
	}
	if len(c.ListModelConfigurations()) != 0 {
		t.Fatal("deleted model still listed")
	}
}

func TestModelAdministrationRejectsInvalidInput(t *testing.T) {
	c, _ := NewModelCatalog(nil, "agent", testRing(t, "v1"))
	cases := []func(*ModelInput){
		func(m *ModelInput) { m.APIKeys = nil },
		func(m *ModelInput) { m.APIKeys[0].Value = nil },
		func(m *ModelInput) { m.APIKeys[0].Weight = 0 },
		func(m *ModelInput) { m.APIKeys[0].Weight = -1 },
		func(m *ModelInput) { m.APIKeys[0].ID = " " },
		func(m *ModelInput) { m.APIKeys = append(m.APIKeys, m.APIKeys[0]) },
		func(m *ModelInput) { m.Options.RequestTimeout = "invalid" },
		func(m *ModelInput) { m.BaseURL = "https://user:password@example.invalid" },
	}
	for i, change := range cases {
		m := adminInput()
		change(&m)
		_, err := c.PutModelConfiguration("model", m, nil, func(*schema.Model, *schema.Model) error { t.Fatal("invalid input persisted"); return nil })
		if !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}
