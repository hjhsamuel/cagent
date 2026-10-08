package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

// ModelInput is a complete replacement. A nil key value preserves the stored
// credential with the same ID; plaintext is encrypted before persistence.
type ModelInput struct {
	Model    string             `json:"model"`
	Provider string             `json:"provider"`
	BaseURL  string             `json:"base_url"`
	APIKeys  []ModelKeyInput    `json:"api_keys"`
	Options  schema.ModelConfig `json:"config"`
}

type ModelKeyInput struct {
	ID     string  `json:"id"`
	Value  *string `json:"value,omitempty"`
	Weight int64   `json:"weight"`
}

type ModelView struct {
	ID       string             `json:"id"`
	Model    string             `json:"model"`
	Provider string             `json:"provider"`
	BaseURL  string             `json:"base_url"`
	APIKeys  []ModelKeyView     `json:"api_keys"`
	Options  schema.ModelConfig `json:"config"`
}

type ModelKeyView struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Weight  int64  `json:"weight"`
}

func (m ModelInput) Format(s fmt.State, _ rune)    { fmt.Fprint(s, "[redacted model input]") }
func (k ModelKeyInput) Format(s fmt.State, _ rune) { fmt.Fprint(s, "[redacted model key input]") }

func modelView(doc schema.Model) ModelView {
	v := ModelView{ID: doc.ID, Model: doc.Model, Provider: doc.Provider, BaseURL: doc.BaseURL, Options: doc.Options, APIKeys: make([]ModelKeyView, 0, len(doc.APIKeys))}
	for _, key := range doc.APIKeys {
		v.APIKeys = append(v.APIKeys, ModelKeyView{StoredKeyID(key), key.Version, key.Weight})
	}
	// Detach nested thinking values from the catalog.
	data, _ := json.Marshal(v)
	var copy ModelView
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (c *ModelCatalog) ListModelConfigurations() []ModelView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	views := make([]ModelView, 0, len(c.documents))
	for _, doc := range c.documents {
		views = append(views, modelView(doc))
	}
	return views
}

func (c *ModelCatalog) GetModelConfiguration(id string) (ModelView, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	doc, ok := c.byID[id]
	if !ok {
		return ModelView{}, apperrors.ErrNotFound
	}
	return modelView(doc), nil
}

// PutModelConfiguration serializes writes with encryption rotation and only
// publishes after the compare-and-swap persistence callback succeeds.
func (c *ModelCatalog) PutModelConfiguration(id string, input ModelInput, validate func(schema.Model) error, persist func(*schema.Model, *schema.Model) error) (ModelView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(id) == "" || persist == nil {
		return ModelView{}, apperrors.ErrInvalidArgument
	}
	doc := schema.Model{ID: id, Model: input.Model, Provider: input.Provider, BaseURL: input.BaseURL, Options: input.Options, APIKeys: make([]schema.EncryptedKey, 0, len(input.APIKeys))}
	old, exists := c.byID[id]
	for _, key := range input.APIKeys {
		if strings.TrimSpace(key.ID) == "" {
			return ModelView{}, apperrors.ErrInvalidArgument
		}
		var stored schema.EncryptedKey
		if key.Value != nil {
			var err error
			stored, err = c.ring.Encrypt(*key.Value, key.Weight)
			if err != nil {
				return ModelView{}, err
			}
			stored.ID = key.ID
		} else {
			found := false
			for _, previous := range old.APIKeys {
				if StoredKeyID(previous) == key.ID {
					stored, found = previous, true
					break
				}
			}
			if !found {
				return ModelView{}, apperrors.ErrInvalidArgument
			}
			stored.ID, stored.Weight = key.ID, key.Weight
		}
		doc.APIKeys = append(doc.APIKeys, stored)
	}
	if _, err := ResolveModel(doc, c.name, c.ring); err != nil {
		return ModelView{}, err
	}
	if validate != nil {
		if err := validate(doc); err != nil {
			return ModelView{}, err
		}
	}
	// Own all nested JSON before handing the document to persistence.
	data, err := json.Marshal(doc)
	if err != nil {
		return ModelView{}, apperrors.ErrInvalidArgument
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ModelView{}, apperrors.ErrInvalidArgument
	}
	var expected *schema.Model
	if exists {
		expected = &old
	}
	if err := persist(expected, &doc); err != nil {
		return ModelView{}, err
	}
	c.byID[id] = doc
	if exists {
		for i := range c.documents {
			if c.documents[i].ID == id {
				c.documents[i] = doc
				break
			}
		}
	} else {
		c.documents = append(c.documents, doc)
	}
	return modelView(doc), nil
}

func (c *ModelCatalog) DeleteModelConfiguration(id string, persist func(*schema.Model, *schema.Model) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.byID[id]
	if !ok {
		return apperrors.ErrNotFound
	}
	if persist == nil {
		return apperrors.ErrInvalidArgument
	}
	if err := persist(&old, nil); err != nil {
		return err
	}
	delete(c.byID, id)
	for i := range c.documents {
		if c.documents[i].ID == id {
			c.documents = append(c.documents[:i], c.documents[i+1:]...)
			break
		}
	}
	return nil
}
