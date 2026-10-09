package bootstrap

import (
	"context"
	"errors"
	"reflect"

	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

type fakeModels map[string]schema.Model

func (f fakeModels) WriteModel(ctx context.Context, old, updated *schema.Model) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if updated == nil {
		delete(f, old.ID)
	} else {
		f[updated.ID] = *updated
	}
	return nil
}

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

	ring, err := config.NewKeyring(config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.Encrypt("test-secret", 1)
	if err != nil {
		t.Fatal(err)
	}
	doc := schema.Model{ID: "main", Model: "vendor-model", Provider: "GLM", BaseURL: "https://example.invalid/v1", APIKeys: []schema.EncryptedKey{key}, Options: schema.ModelConfig{WindowTokens: 32768, RequestTimeout: "30s", Thinking: schema.Thinking{Enabled: true, Key: "thinking.type", Value: "enabled"}}}
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

func TestMongoModelCatalogAllowsEmptyUntilUse(t *testing.T) {
	c, ring, _ := modelFixture(t)
	loaded, err := loadModels(context.Background(), fakeModels{}, c, ring)
	if err != nil {
		t.Fatal("empty model catalog prevented startup", err)
	}
	if loaded.Models == nil {
		t.Fatal("empty model catalog was not initialized")
	}
	if err := loaded.Validate(); err != nil {
		t.Fatal("empty model catalog invalidated service config", err)
	}
	if _, err := loaded.Models.Select(""); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("selection without models must fail", err)
	}
	if _, err := loaded.Models.Bind("missing", "key"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("missing bound model must fail", err)
	}
}

func TestMongoModelCatalogFailsClosed(t *testing.T) {
	for _, mode := range []string{"timeout", "thinking", "missing_window", "negative_window"} {
		t.Run(mode, func(t *testing.T) {
			c, ring, docs := modelFixture(t)
			d := docs["main"]
			switch mode {
			case "missing_window":
				d.Options.WindowTokens = 0
			case "negative_window":
				d.Options.WindowTokens = -1
			case "bad_ciphertext":
				d.APIKeys[0].Ciphertext = "bad"
			case "missing_nonce":
				d.APIKeys[0].Nonce = ""
			case "unknown_version":
				d.APIKeys[0].Version = "missing"
			case "timeout":
				d.Options.RequestTimeout = "0s"
			case "thinking":
				d.Options.Thinking.Key = "messages"
			}
			docs["main"] = d
			loaded, err := loadModels(context.Background(), docs, c, ring)
			if err == nil || !reflect.DeepEqual(loaded, config.Config{}) {
				t.Fatal("invalid directory returned partial config")
			}
		})
	}
}

func TestModelManagementValidatesAndPublishes(t *testing.T) {
	cfg, ring, docs := modelFixture(t)
	loaded, err := loadModels(context.Background(), docs, cfg, ring)
	if err != nil {
		t.Fatal(err)
	}
	m := modelManagement{cfg: loaded, db: docs}
	doc := docs["main"]
	input := config.ModelInput{Model: "updated-vendor", Provider: doc.Provider, BaseURL: doc.BaseURL, Options: doc.Options, APIKeys: []config.ModelKeyInput{{ID: config.StoredKeyID(doc.APIKeys[0]), Weight: 1}}}
	input.Options.RequestTimeout = "0s"
	if _, err := m.Put(context.Background(), "main", input); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("invalid timeout accepted", err)
	}
	if docs["main"].Model != doc.Model {
		t.Fatal("invalid configuration persisted")
	}
	input.Options.RequestTimeout = doc.Options.RequestTimeout
	if _, err := m.Put(context.Background(), "main", input); err != nil {
		t.Fatal(err)
	}
	selected, err := loaded.Models.Bind("main", input.APIKeys[0].ID)
	if err != nil || selected.Agent.Model != "updated-vendor" {
		t.Fatal("runtime did not receive update", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input.Model = "canceled"
	if _, err := m.Put(ctx, "main", input); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got, _ := m.Get(context.Background(), "main"); got.Model != "updated-vendor" {
		t.Fatal("canceled write published")
	}
}
