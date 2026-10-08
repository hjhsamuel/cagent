package bootstrap

import (
	"context"

	"testing"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

type fakeModels map[string]schema.Model

func (f fakeModels) ListModels(_ context.Context) ([]schema.Model, error) {
	var out []schema.Model
	for _, doc := range f {
		out = append(out, doc)
	}
	return out, nil
}

func modelFixture(t *testing.T) (config.Config, *config.Keyring, fakeModels) {
	t.Helper()
	c := config.Defaults()
	c.MongoDB.URI = "mongodb://localhost:27017"

	ring, err := config.NewKeyring(config.ModelEncryption{KeysJSON: `{"v1":"MDEyMzQ1Njc4OWFiY2RlZg=="}`, ActiveVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.Encrypt("test-secret", 1)
	if err != nil {
		t.Fatal(err)
	}
	doc := schema.Model{ID: "main", Model: "vendor-model", Provider: "GLM", BaseURL: "https://example.invalid/v1", APIKeys: []schema.EncryptedKey{key}, Options: schema.ModelConfig{TokenEncoding: "o200k_base", MaxTokensField: "max_completion_tokens", RequestTimeout: "30s", WindowTokens: 32000, OutputTokens: 4096, Thinking: schema.Thinking{Enabled: true, Key: "thinking.type", Value: "enabled"}}}
	return c, ring, fakeModels{"main": doc}
}

func TestMongoModelCatalogHasNoDefault(t *testing.T) {
	c, ring, docs := modelFixture(t)
	other := docs["main"]
	other.ID = "other"
	other.Model = "other-model"
	other.Provider = "DeepSeek"
	docs["other"] = other
	loaded, err := loadModels(context.Background(), docs, c, ring)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent.Model != "" || loaded.Agent.APIKey != "" || loaded.Models == nil || loaded.SummaryAgent != nil {
		t.Fatal("startup selected a default model")
	}
	selected, err := loaded.Models.Select("other")
	if err != nil || selected.Agent.Model != "other-model" || selected.Agent.Provider != "DeepSeek" {
		t.Fatal("model directory not loaded", err)
	}
}

func TestMongoModelCatalogFailsClosed(t *testing.T) {
	for _, mode := range []string{"empty", "bad_ciphertext", "missing_nonce", "unknown_version", "budget", "timeout", "thinking"} {
		t.Run(mode, func(t *testing.T) {
			c, ring, docs := modelFixture(t)
			d := docs["main"]
			switch mode {
			case "empty":
				docs = fakeModels{}
			case "bad_ciphertext":
				d.APIKeys[0].Ciphertext = "bad"
			case "missing_nonce":
				d.APIKeys[0].Nonce = ""
			case "unknown_version":
				d.APIKeys[0].Version = "missing"
			case "budget":
				d.Options.WindowTokens = 5000
			case "timeout":
				d.Options.RequestTimeout = "0s"
			case "thinking":
				d.Options.Thinking.Key = "messages"
			}
			if mode != "empty" {
				docs["main"] = d
			}
			loaded, err := loadModels(context.Background(), docs, c, ring)
			if err == nil || loaded != (config.Config{}) {
				t.Fatal("invalid directory returned partial config")
			}
		})
	}
}
