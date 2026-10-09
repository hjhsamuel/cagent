package config

import (
	"crypto/rand"
	"encoding/base64"
	"math"
	"strconv"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/pkg/kms"
)

// 默认生成 32 字节 AES-256 密钥，Base64 仅用于持久化编码。
const defaultModelEncryptionKeyBytes = kms.KeyLength

// RotateKeys 仅在调用时生成服务端密钥；持久化成功后发布新目录，保留稳定凭据引用。
func (c *ModelCatalog) RotateKeys(persistKeys func(ModelEncryption) error, persistModels func([]schema.Model, []schema.Model) error) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ring == nil || c.ring.deferred == nil || persistKeys == nil || persistModels == nil {
		return "", invalid("model_encryption", "rotation unavailable")
	}
	encryption := ModelEncryption{Keys: map[string]string{}}
	var latest uint64
	for version, value := range c.ring.deferred.Keys {
		n, err := encryptionVersion(version)
		if err != nil {
			return "", err
		}
		latest = max(latest, n)
		encryption.Keys[version] = value
	}
	if latest == math.MaxUint64 {
		return "", invalid("model_encryption", "version exhausted")
	}
	version := "v" + strconv.FormatUint(latest+1, 10)
	material := make([]byte, defaultModelEncryptionKeyBytes)
	if _, err := rand.Read(material); err != nil {
		return "", invalid("model_encryption", "cannot generate key")
	}
	encryption.Keys[version] = base64.StdEncoding.EncodeToString(material)
	ring, err := NewKeyring(encryption)
	if err != nil {
		return "", err
	}
	docs := make([]schema.Model, len(c.documents))
	for i, old := range c.documents {
		docs[i] = old
		docs[i].APIKeys = make([]schema.EncryptedKey, len(old.APIKeys))
		for j, key := range old.APIKeys {
			plain, err := c.ring.Decrypt(key)
			if err != nil {
				return "", err
			}
			rotated, err := ring.Encrypt(plain, key.Weight)
			if err != nil {
				return "", err
			}
			rotated.ID = StoredKeyID(key)
			docs[i].APIKeys[j] = rotated
		}
	}
	if err := persistKeys(encryption); err != nil {
		return "", err
	}
	// 已落盘的版本始终保留；数据库失败后重试使用下一版本。
	c.ring = NewDeferredKeyring(encryption)
	if err := persistModels(c.documents, docs); err != nil {
		return "", err
	}
	c.documents = docs
	c.byID = make(map[string]schema.Model, len(docs))
	for _, doc := range docs {
		c.byID[doc.ID] = doc
	}
	c.ring = NewDeferredKeyring(encryption)
	return version, nil
}
