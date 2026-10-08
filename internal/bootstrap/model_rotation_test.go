package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/joho/godotenv"
)

func TestKeysValidatedOnUse(t *testing.T) {
	for _, mode := range []string{"missing", "invalid_aes", "ciphertext", "nonce", "version"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, docs := modelFixture(t)
			cfg.ModelEncryption.Keys = map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}
			doc := docs["main"]
			switch mode {
			case "missing":
				cfg.ModelEncryption.Keys = nil
			case "invalid_aes":
				cfg.ModelEncryption.Keys["v1"] = "bad"
			case "ciphertext":
				doc.APIKeys[0].Ciphertext = "bad"
			case "nonce":
				doc.APIKeys[0].Nonce = ""
			case "version":
				doc.APIKeys[0].Version = "v99"
			}
			docs["main"] = doc
			loaded, err := loadModels(context.Background(), docs, cfg, config.NewDeferredKeyring(cfg.ModelEncryption))
			if err != nil {
				t.Fatal("startup validated keys", err)
			}
			if _, err := loaded.Models.Select("main"); err == nil {
				t.Fatal("invalid credential used")
			}
			if _, err := loaded.Models.Bind("main", config.StoredKeyID(doc.APIKeys[0])); err == nil {
				t.Fatal("invalid binding used")
			}
		})
	}
}

func TestPersistModelKeysPreservesEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	const before = "# config\nOTHER=keep\nCAGENT_MODEL_ENCRYPTION_KEY_V1=MDEyMzQ1Njc4OWFiY2RlZg==\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	keys := config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg==", "v2": "ZmVkY2JhOTg3NjU0MzIxMA=="}}
	if err := persistModelKeys(path, keys); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), before) {
		t.Fatal("unrelated env modified")
	}
	values, err := godotenv.Read(path)
	if err != nil || values["CAGENT_MODEL_ENCRYPTION_KEY_V2"] != keys.Keys["v2"] {
		t.Fatal("key not persisted", err)
	}
	keys.Keys["v2"] = "different"
	if err := persistModelKeys(path, keys); err == nil {
		t.Fatal("version overwritten")
	}
}

func TestRotationPersistenceAndRetry(t *testing.T) {
	cfg, _, docs := modelFixture(t)
	encryption := config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}}
	loaded, err := loadModels(context.Background(), docs, cfg, config.NewDeferredKeyring(encryption))
	if err != nil {
		t.Fatal(err)
	}
	id := config.StoredKeyID(docs["main"].APIKeys[0])
	path := filepath.Join(t.TempDir(), ".env")
	var saved config.ModelEncryption
	persist := func(keys config.ModelEncryption) error { saved = keys; return persistModelKeys(path, keys) }
	failure := func(_, _ []schema.Model) error { return errors.New("database failure") }
	if _, err := loaded.Models.RotateKeys(persist, failure); err == nil {
		t.Fatal("db failure ignored")
	}
	if bound, err := loaded.Models.Bind("main", id); err != nil || bound.Agent.APIKey != "test-secret" {
		t.Fatal("failure lost old binding", err)
	}
	version, err := loaded.Models.RotateKeys(persist, func(old, updated []schema.Model) error {
		if updated[0].APIKeys[0].ID != id || updated[0].APIKeys[0].Weight != old[0].APIKeys[0].Weight {
			t.Fatal("binding changed")
		}
		ring, err := config.NewKeyring(saved)
		if err != nil {
			t.Fatal(err)
		}
		if plain, err := ring.Decrypt(updated[0].APIKeys[0]); err != nil || plain != "test-secret" {
			t.Fatal("rotation corrupted key", err)
		}
		return nil
	})
	if err != nil || version != "v3" {
		t.Fatal("retry failed", err)
	}
	if bound, err := loaded.Models.Bind("main", id); err != nil || bound.Agent.APIKey != "test-secret" {
		t.Fatal("rotation lost binding", err)
	}
	called := false
	_, err = loaded.Models.RotateKeys(func(config.ModelEncryption) error { return errors.New("env unwritable") }, func(_, _ []schema.Model) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("db updated before env persisted")
	}
}
