package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

// ModelEncryption 的 keyring 为 JSON {版本: base64(AES密钥)}，禁止整体打印。
type ModelEncryption struct {
	KeysJSON      string
	ActiveVersion string
}

// Keyring 不暴露密钥材料。版本作为附加认证数据，禁止篡改版本后解密。
type Keyring struct {
	keys   map[string]cipher.AEAD
	active string
}

func NewKeyring(c ModelEncryption) (*Keyring, error) {
	var encoded map[string]string
	if json.Unmarshal([]byte(c.KeysJSON), &encoded) != nil || len(encoded) == 0 {
		return nil, invalid("model_encryption.keys", "must be a nonempty JSON object of versioned base64 AES keys")
	}
	k := &Keyring{keys: make(map[string]cipher.AEAD), active: c.ActiveVersion}
	for version, value := range encoded {
		if strings.TrimSpace(version) == "" {
			return nil, invalid("model_encryption.keys", "version must not be blank")
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || (len(key) != 16 && len(key) != 24 && len(key) != 32) {
			return nil, invalid("model_encryption.keys", "AES keys must encode 16, 24 or 32 bytes")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, invalid("model_encryption.keys", "invalid AES key")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, invalid("model_encryption.keys", "invalid AES-GCM key")
		}
		k.keys[version] = aead
	}
	if k.keys[k.active] == nil {
		return nil, invalid("model_encryption.active_version", "must identify a configured key version")
	}
	return k, nil
}

func (k *Keyring) Encrypt(plaintext string, weight int64) (schema.EncryptedKey, error) {
	if k == nil || k.keys[k.active] == nil || strings.TrimSpace(plaintext) == "" || weight < 0 {
		return schema.EncryptedKey{}, invalid("model.api_keys", "invalid encryption input")
	}
	aead := k.keys[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return schema.EncryptedKey{}, invalid("model.api_keys", "cannot generate encryption nonce")
	}
	sealed := aead.Seal(nil, nonce, []byte(plaintext), []byte(k.active))
	key := schema.EncryptedKey{Version: k.active, Ciphertext: base64.StdEncoding.EncodeToString(sealed), Nonce: base64.StdEncoding.EncodeToString(nonce), Weight: weight}
	key.ID = StoredKeyID(key)
	return key, nil
}

// StoredKeyID 为无 ID 的旧记录生成不含明文的稳定引用；轮换时必须保留此 ID。
func StoredKeyID(key schema.EncryptedKey) string {
	if key.ID != "" {
		return key.ID
	}
	digest := sha256.Sum256([]byte(key.Version + "\x00" + key.Nonce + "\x00" + key.Ciphertext))
	return hex.EncodeToString(digest[:])
}

func (k *Keyring) Decrypt(key schema.EncryptedKey) (string, error) {
	if k == nil || k.keys[key.Version] == nil {
		return "", invalid("model.api_keys", "unknown encryption key version")
	}
	aead := k.keys[key.Version]
	nonce, err := base64.StdEncoding.DecodeString(key.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", invalid("model.api_keys.nonce", "nonce must be base64 encoded with the AES-GCM nonce size")
	}
	data, err := base64.StdEncoding.DecodeString(key.Ciphertext)
	if err != nil || len(data) < aead.Overhead() {
		return "", invalid("model.api_keys", "invalid encrypted API key")
	}
	plain, err := aead.Open(nil, nonce, data, []byte(key.Version))
	if err != nil || strings.TrimSpace(string(plain)) == "" {
		return "", invalid("model.api_keys", "API key authentication failed")
	}
	return string(plain), nil
}

type weightedKey struct {
	id     string
	value  string
	weight int64
}

// KeyPool 初始化后只读，可在并发请求中复用；权重 0 暂停使用但仍验证密文。
type KeyPool struct {
	keys  []weightedKey
	total int64
}

func NewKeyPool(keys []schema.EncryptedKey, ring *Keyring) (*KeyPool, error) {
	p := &KeyPool{}
	for _, key := range keys {
		if key.Weight < 0 || key.Weight > math.MaxInt64-p.total {
			return nil, invalid("model.api_keys.weight", "invalid or overflowing weight")
		}
		value, err := ring.Decrypt(key)
		if err != nil {
			return nil, err
		}
		if key.Weight > 0 {
			p.keys = append(p.keys, weightedKey{id: StoredKeyID(key), value: value, weight: key.Weight})
			p.total += key.Weight
		}
	}
	if p.total == 0 {
		return nil, invalid("model.api_keys", "at least one key must have positive weight")
	}
	return p, nil
}

func (p *KeyPool) Pick() (string, error) {
	if p == nil || p.total <= 0 {
		return "", invalid("model.api_keys", "empty API key pool")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(p.total))
	if err != nil {
		return "", invalid("model.api_keys", "cannot select API key")
	}
	return p.pick(n.Int64()), nil
}

func (p *KeyPool) pick(n int64) string {
	for _, key := range p.keys {
		if n < key.weight {
			return key.value
		}
		n -= key.weight
	}
	return ""
}

func (k *Keyring) Format(s fmt.State, _ rune)        { fmt.Fprint(s, "[redacted keyring]") }
func (p *KeyPool) Format(s fmt.State, _ rune)        { fmt.Fprint(s, "[redacted API key pool]") }
func (c ModelEncryption) Format(s fmt.State, _ rune) { fmt.Fprint(s, "[redacted model encryption]") }
