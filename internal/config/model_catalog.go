package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

// ModelCatalog 校验模型参数，使用时校验凭据，并支持并发读取和串行轮换。
type ModelCatalog struct {
	mu        sync.RWMutex
	documents []schema.Model
	byID      map[string]schema.Model
	ring      *Keyring
	name      string
}

type SelectedModel struct {
	ModelID  string
	APIKeyID string
	Agent    Agent
	Options  schema.ModelConfig
}

func NewModelCatalog(docs []schema.Model, name string, ring *Keyring) (*ModelCatalog, error) {
	c := &ModelCatalog{byID: make(map[string]schema.Model), ring: ring, name: name}
	for _, doc := range docs {
		// 隔离调用方修改文档、切片和嵌套思考配置。
		data, err := json.Marshal(doc)
		if err != nil {
			return nil, invalid("models", "invalid model document")
		}
		var copy schema.Model
		if err := json.Unmarshal(data, &copy); err != nil {
			return nil, invalid("models", "invalid model document")
		}
		if _, exists := c.byID[copy.ID]; exists {
			return nil, invalid("models", "duplicate model id")
		}
		if _, err := ResolveModelMetadata(copy, name); err != nil {
			return nil, err
		}
		c.documents = append(c.documents, copy)
		c.byID[copy.ID] = copy
	}
	return c, nil
}

// Select 未指定模型时均匀抽取模型，再按该模型的 key 权重随机抽取凭据。
func (c *ModelCatalog) Select(modelID string) (SelectedModel, error) {
	if c != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
	}
	if c == nil || len(c.documents) == 0 {
		return SelectedModel{}, invalid("models", "model catalog is unavailable")
	}
	if modelID == "" {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(c.documents))))
		if err != nil {
			return SelectedModel{}, invalid("models", "cannot select model")
		}
		modelID = c.documents[n.Int64()].ID
	}
	doc, ok := c.byID[modelID]
	if !ok {
		return SelectedModel{}, apperrors.New(apperrors.ErrNotFound, "model.id", "selected model is unavailable")
	}
	a, err := ResolveModel(doc, c.name, c.ring)
	if err != nil {
		return SelectedModel{}, err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(a.Keys.total))
	if err != nil {
		return SelectedModel{}, invalid("model.api_keys", "cannot select API key")
	}
	v := n.Int64()
	for _, key := range a.Keys.keys {
		if v < key.weight {
			return c.bind(modelID, key.id)
		}
		v -= key.weight
	}
	return SelectedModel{}, invalid("model.api_keys", "cannot select API key")
}

// Bind 精确解析持久引用；不会在模型或凭据缺失时重新随机选择。
func (c *ModelCatalog) Bind(modelID, keyID string) (SelectedModel, error) {
	if c != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
	}
	return c.bind(modelID, keyID)
}

func (c *ModelCatalog) bind(modelID, keyID string) (SelectedModel, error) {
	if c == nil {
		return SelectedModel{}, invalid("models", "model catalog is unavailable")
	}
	doc, ok := c.byID[modelID]
	if !ok {
		return SelectedModel{}, apperrors.New(apperrors.ErrNotFound, "model.id", "bound model is unavailable")
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return SelectedModel{}, invalid("models", "invalid model document")
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return SelectedModel{}, invalid("models", "invalid model document")
	}
	for _, key := range doc.APIKeys {
		if StoredKeyID(key) != keyID {
			continue
		}
		plain, err := c.ring.Decrypt(key)
		if err != nil {
			return SelectedModel{}, err
		}
		a, err := ResolveModel(doc, c.name, c.ring)
		if err != nil {
			return SelectedModel{}, err
		}
		a.Keys, a.APIKey = nil, plain
		return SelectedModel{ModelID: modelID, APIKeyID: keyID, Agent: a, Options: doc.Options}, nil
	}
	return SelectedModel{}, apperrors.New(apperrors.ErrNotFound, "model.api_key_id", "bound API key is unavailable")
}

func (c *ModelCatalog) Format(s fmt.State, _ rune) { fmt.Fprint(s, "[model catalog]") }
