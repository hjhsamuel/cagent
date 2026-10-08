package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func testRing(t *testing.T, active string) *Keyring {
	t.Helper()
	keys := map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}
	if active == "v2" {
		keys["v2"] = "ZmVkY2JhOTg3NjU0MzIxMA=="
	}
	k, err := NewKeyring(ModelEncryption{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAESGCMRotationAndAuthentication(t *testing.T) {
	old, current := testRing(t, "v1"), testRing(t, "v2")
	a, err := old.Encrypt("api-secret", 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := old.Encrypt("api-secret", 7)
	if err != nil {
		t.Fatal(err)
	}
	if a.Ciphertext == b.Ciphertext || a.Nonce == b.Nonce || a.Version != "v1" {
		t.Fatal("nonce or version invalid")
	}
	plain, err := current.Decrypt(a)
	if err != nil || plain != "api-secret" {
		t.Fatal("old version cannot be decrypted", err)
	}
	rotated, err := current.Encrypt(plain, a.Weight)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version != "v2" || rotated.Weight != 7 {
		t.Fatal("rotation lost version or weight")
	}
	plain, err = current.Decrypt(rotated)
	if err != nil || plain != "api-secret" {
		t.Fatal("rotation lost API key")
	}
	raw, _ := base64.StdEncoding.DecodeString(a.Ciphertext)
	raw[len(raw)-1] ^= 1
	for _, bad := range []schema.EncryptedKey{
		{Version: "unknown", Ciphertext: a.Ciphertext, Nonce: a.Nonce},
		{Version: "v2", Ciphertext: a.Ciphertext, Nonce: a.Nonce},
		{Version: "v1", Ciphertext: "bad", Nonce: a.Nonce},
		{Version: "v1", Ciphertext: base64.StdEncoding.EncodeToString(raw), Nonce: a.Nonce},
		{Version: "v1", Ciphertext: a.Ciphertext},
		{Version: "v1", Ciphertext: a.Ciphertext, Nonce: "bad"},
		{Version: "v1", Ciphertext: a.Ciphertext, Nonce: base64.StdEncoding.EncodeToString([]byte("short"))},
		{Version: "v1", Ciphertext: a.Ciphertext, Nonce: b.Nonce},
	} {
		value, err := current.Decrypt(bad)
		if err == nil || value != "" || strings.Contains(fmt.Sprintf("%+v", err), "api-secret") {
			t.Fatal("invalid ciphertext accepted or leaked")
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", current), "MDEy") {
		t.Fatal("keyring leaked")
	}
}

func TestKeyringRejectsInvalidConfig(t *testing.T) {
	for _, cfg := range []ModelEncryption{{}, {Keys: map[string]string{}}, {Keys: map[string]string{"v1": "secret-invalid-base64"}}, {Keys: map[string]string{"v1": "YWJj"}}, {Keys: map[string]string{"missing": "MDEyMzQ1Njc4OWFiY2RlZg=="}}, {Keys: map[string]string{"v01": "MDEyMzQ1Njc4OWFiY2RlZg=="}}, {Keys: map[string]string{"v0": "MDEyMzQ1Njc4OWFiY2RlZg=="}}} {
		if ring, err := NewKeyring(cfg); err == nil || ring != nil {
			t.Fatal("invalid keyring accepted")
		}
	}
}

func TestVersionedEnvironmentAndNumericOrdering(t *testing.T) {
	values := map[string]string{
		"CAGENT_MODEL_ENCRYPTION_KEY_V2":  "MDEyMzQ1Njc4OWFiY2RlZg==",
		"CAGENT_MODEL_ENCRYPTION_KEY_V10": "ZmVkY2JhOTg3NjU0MzIxMA==",
	}
	reads := map[string]int{}
	lookup := func(name string) (string, bool) {
		reads[name]++
		value, ok := values[name]
		return value, ok
	}
	c, err := LoadModelEncryptionFromEnv(lookup, []string{"PATH=ignored", "CAGENT_MODEL_ENCRYPTION_KEY_V10=ignored", "CAGENT_MODEL_ENCRYPTION_KEY_V2", "CAGENT_MODEL_ENCRYPTION_KEY_V2"})
	if err != nil || len(c.Keys) != 2 || reads["CAGENT_MODEL_ENCRYPTION_KEY_V2"] != 1 || reads["PATH"] != 0 {
		t.Fatal("versioned environment not loaded once", err)
	}
	ring, err := NewKeyring(c)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.Encrypt("api-secret", 1)
	if err != nil || key.Version != "v10" {
		t.Fatal("highest numeric version not selected", err)
	}
	old, err := NewKeyring(ModelEncryption{Keys: map[string]string{"v2": values["CAGENT_MODEL_ENCRYPTION_KEY_V2"]}})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := old.Encrypt("api-secret", 1)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := ring.Decrypt(legacy); err != nil || plain != "api-secret" {
		t.Fatal("older nonconsecutive version cannot decrypt", err)
	}
	values["CAGENT_MODEL_ENCRYPTION_KEY_V10"] = ""
	c, err = LoadModelEncryptionFromEnv(lookup, []string{"CAGENT_MODEL_ENCRYPTION_KEY_V2", "CAGENT_MODEL_ENCRYPTION_KEY_V10"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewKeyring(c); err == nil {
		t.Fatal("empty newest key silently fell back")
	}
}

func TestVersionedEnvironmentRejectsMalformedSuffix(t *testing.T) {
	for _, suffix := range []string{"", "0", "01", "-1", "1x", "18446744073709551616"} {
		cfg, err := LoadModelEncryptionFromEnv(func(string) (string, bool) { return "secret-input", true }, []string{modelEncryptionKeyPrefix + suffix})
		if err != nil {
			t.Fatal("loading must defer key validation", err)
		}
		_, err = NewKeyring(cfg)
		if err == nil || strings.Contains(err.Error(), "secret-input") {
			t.Fatal("invalid suffix accepted or leaked")
		}
	}
	if _, err := LoadModelEncryptionFromEnv(nil, nil); err == nil {
		t.Fatal("nil lookup accepted")
	}
}

func TestWeightedSelectionBoundariesAndConcurrentUse(t *testing.T) {
	ring := testRing(t, "v1")
	a, _ := ring.Encrypt("first", 1)
	b, _ := ring.Encrypt("second", 3)
	disabled, _ := ring.Encrypt("disabled", 0)
	pool, err := NewKeyPool([]schema.EncryptedKey{a, b, disabled}, ring)
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range []string{"first", "second", "second", "second"} {
		if pool.pick(int64(n)) != want {
			t.Fatal("incorrect weighted bucket")
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				key, err := pool.Pick()
				if err != nil || (key != "first" && key != "second") {
					t.Error("invalid concurrent selection", err)
				}
			}
		})
	}
	wg.Wait()
	if strings.Contains(fmt.Sprintf("%+v", pool), "first") {
		t.Fatal("key pool leaked")
	}
	negative := a
	negative.Weight = -1
	overflow := a
	overflow.Weight = math.MaxInt64
	for _, keys := range [][]schema.EncryptedKey{nil, {disabled}, {negative}, {overflow, b}} {
		if p, err := NewKeyPool(keys, ring); err == nil || p != nil {
			t.Fatal("invalid weights accepted")
		}
	}
}

func TestThinkingRejectsRequestOverrides(t *testing.T) {
	for _, key := range []string{"", "thinking..type", "messages", "model.name", "stream", "max_tokens", "thinking.*"} {
		if err := (Thinking{Enabled: true, Key: key, Value: true}).Validate(); err == nil {
			t.Fatalf("invalid thinking key accepted: %s", key)
		}
	}
	for _, key := range []string{"thinking.type", "enable_thinking", "reasoning_effort"} {
		if err := (Thinking{Enabled: true, Key: key, Value: "enabled"}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
