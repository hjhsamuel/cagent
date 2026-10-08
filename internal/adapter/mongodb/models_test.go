package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestModelBSONPreservesThinkingObject(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"_id": "model", "provider": "GLM", "api_keys": bson.A{bson.M{"version": "v1", "ciphertext": "encrypted", "nonce": "separate-nonce", "weight": int64(3)}}, "config": bson.M{"window_tokens": 32000, "thinking": bson.M{"enabled": true, "key": "thinking", "value": bson.M{"type": "enabled"}}}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := decodeModel(raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(d.Options.Thinking.Value)
	if err != nil || string(value) != `{"type":"enabled"}` {
		t.Fatal("BSON object changed JSON shape", err)
	}
	if d.ID != "model" || d.Provider != "GLM" || d.APIKeys[0].Weight != 3 || d.APIKeys[0].Nonce != "separate-nonce" || d.Options.WindowTokens != 32000 {
		t.Fatal("model fields lost")
	}
}

func TestGetModelFromMongoDB(t *testing.T) {
	db, _ := testDatabase(t)
	doc := schema.Model{ID: "model", Model: "vendor-model", Provider: "DeepSeek", APIKeys: []schema.EncryptedKey{{Version: "v1", Ciphertext: "encrypted", Nonce: "separate-nonce", Weight: 2}}}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bson.Raw(raw).Lookup("protocol").Type != 0 {
		t.Fatal("model BSON still stores protocol")
	}
	if _, err := db.collection(ModelCollection).InsertOne(context.Background(), doc); err != nil {
		t.Fatal(safeError(err))
	}
	got, err := db.GetModel(context.Background(), "model")
	if err != nil || got.Provider != "DeepSeek" || got.APIKeys[0].Weight != 2 || got.APIKeys[0].Nonce != "separate-nonce" {
		t.Fatal("model not loaded", err)
	}
	if _, err := db.GetModel(context.Background(), "missing"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("missing model did not fail", err)
	}
}
