package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func TestEncryptAndRotateCLI(t *testing.T) {
	active := "v1"
	keys := `{"v1":"MDEyMzQ1Njc4OWFiY2RlZg==","v2":"ZmVkY2JhOTg3NjU0MzIxMA=="}`
	lookup := func(name string) (string, bool) {
		if name == "CAGENT_MODEL_ENCRYPTION_KEYS" {
			return keys, true
		}
		return active, true
	}
	var output bytes.Buffer
	if err := run(strings.NewReader("secret-input\n"), &output, 3, false, false, lookup); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret-input") {
		t.Fatal("CLI leaked plaintext")
	}
	old := output.String()
	var original schema.EncryptedKey
	if err := json.Unmarshal([]byte(old), &original); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	active = "v2"
	if err := run(strings.NewReader(old), &output, 1, true, false, lookup); err != nil {
		t.Fatal(err)
	}
	var encrypted schema.EncryptedKey
	if err := json.Unmarshal(output.Bytes(), &encrypted); err != nil {
		t.Fatal(err)
	}
	ring, err := config.NewKeyring(config.ModelEncryption{KeysJSON: keys, ActiveVersion: active})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ring.Decrypt(encrypted)
	if err != nil || plain != "secret-input" || encrypted.Version != "v2" || encrypted.Weight != 3 || original.ID == "" || encrypted.ID != original.ID {
		t.Fatal("CLI rotation failed", err)
	}
}

func TestExplicitLegacyCiphertextMigration(t *testing.T) {
	keys := `{"v1":"MDEyMzQ1Njc4OWFiY2RlZg=="}`
	lookup := func(name string) (string, bool) {
		if name == "CAGENT_MODEL_ENCRYPTION_KEYS" {
			return keys, true
		}
		return "v1", true
	}
	ring, err := config.NewKeyring(config.ModelEncryption{KeysJSON: keys, ActiveVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.Encrypt("legacy-secret", 5)
	if err != nil {
		t.Fatal(err)
	}
	nonce, _ := base64.StdEncoding.DecodeString(key.Nonce)
	ciphertext, _ := base64.StdEncoding.DecodeString(key.Ciphertext)
	key.Ciphertext = base64.StdEncoding.EncodeToString(append(nonce, ciphertext...))
	key.Nonce = ""
	old, _ := json.Marshal(key)
	var output bytes.Buffer
	if err := run(bytes.NewReader(old), &output, 1, true, false, lookup); err == nil {
		t.Fatal("runtime accepted old format without explicit migration")
	}
	if err := run(bytes.NewReader(old), &output, 1, true, true, lookup); err != nil {
		t.Fatal(err)
	}
	var migrated schema.EncryptedKey
	if err := json.Unmarshal(output.Bytes(), &migrated); err != nil {
		t.Fatal(err)
	}
	plain, err := ring.Decrypt(migrated)
	if err != nil || plain != "legacy-secret" || migrated.Nonce == "" || migrated.Weight != 5 {
		t.Fatal("legacy migration failed", err)
	}
	if err := run(bytes.NewReader(old), &output, 1, false, true, lookup); err == nil {
		t.Fatal("legacy flag accepted without rotation")
	}
}
