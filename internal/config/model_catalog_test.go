package config

import (
	"errors"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func TestCatalogSelectionAndStableBindings(t *testing.T) {
	ring := testRing(t, "v1")
	key, err := ring.Encrypt("first-secret", 1)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := ring.Encrypt("disabled-secret", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ring.Encrypt("second-secret", 3)
	if err != nil {
		t.Fatal(err)
	}
	doc := schema.Model{ID: "first", Model: "vendor-first", Provider: "GLM", BaseURL: "https://example.invalid/v1", APIKeys: []schema.EncryptedKey{key, disabled, second}, Options: schema.ModelConfig{TokenEncoding: "o200k_base", MaxTokensField: "max_tokens", RequestTimeout: "2s", WindowTokens: 8192, OutputTokens: 2048}}
	other := doc
	other.ID, other.Model = "second", "vendor-second"
	c, err := NewModelCatalog([]schema.Model{doc, other}, "agent", ring)
	if err != nil {
		t.Fatal(err)
	}
	// Changing the caller's source must not alter the startup snapshot.
	doc.APIKeys[0].ID = "mutated"
	seenModels, seenKeys := map[string]bool{}, map[string]bool{}
	for i := 0; i < 100; i++ {
		selected, err := c.Select("")
		if err != nil {
			t.Fatal(err)
		}
		seenModels[selected.ModelID], seenKeys[selected.APIKeyID] = true, true
		if selected.APIKeyID == disabled.ID || selected.Agent.Keys != nil {
			t.Fatal("disabled or unbound key selected")
		}
		bound, err := c.Bind(selected.ModelID, selected.APIKeyID)
		if err != nil || bound.Agent.APIKey != selected.Agent.APIKey {
			t.Fatal("binding changed", err)
		}
		explicit, err := c.Select("first")
		if err != nil || explicit.ModelID != "first" || explicit.Agent.Model != "vendor-first" {
			t.Fatal("explicit model ignored", err)
		}
	}
	if len(seenModels) != 2 || len(seenKeys) != 2 {
		t.Fatal("random choices did not cover available models and keys")
	}
	if _, err := c.Select("missing"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("unknown model accepted", err)
	}
	if _, err := c.Bind("first", "missing"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("missing key silently replaced", err)
	}
	// Zero weight prevents new selection but keeps existing sessions usable.
	bound, err := c.Bind("first", disabled.ID)
	if err != nil || bound.Agent.APIKey != "disabled-secret" {
		t.Fatal("existing binding lost", err)
	}
}
