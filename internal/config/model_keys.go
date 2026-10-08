package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

// ModelEncryption 保存各版本的 Base64 AES 密钥，禁止整体打印。
type ModelEncryption struct {
	Keys map[string]string
}

const modelEncryptionKeyPrefix = "CAGENT_MODEL_ENCRYPTION_KEY_V"

// LoadModelEncryptionFromEnv 根据环境名称列表读取独立版本密钥。
// names 可包含 os.Environ 返回的 name=value 项；值只通过 lookup 读取。
func LoadModelEncryptionFromEnv(lookup func(string) (string, bool), names []string) (ModelEncryption, error) {
	if lookup == nil {
		return ModelEncryption{}, invalid("environment", "environment lookup is required")
	}
	c := ModelEncryption{}
	seen := make(map[string]bool)
	for _, entry := range names {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, modelEncryptionKeyPrefix) || seen[name] {
			continue
		}
		seen[name] = true
		suffix := strings.TrimPrefix(name, modelEncryptionKeyPrefix)
		if value, present := lookup(name); present {
			if c.Keys == nil {
				c.Keys = make(map[string]string)
			}
			c.Keys["v"+suffix] = value
		}
	}
	return c, nil
}

func encryptionVersion(version string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(version, "v"), 10, 64)
	if err != nil || n == 0 || version != "v"+strconv.FormatUint(n, 10) {
		return 0, invalid("model_encryption.keys", "version must be v followed by a positive decimal integer without leading zeros")
	}
	return n, nil
}

// Keyring 不暴露密钥材料。版本作为附加认证数据，禁止篡改版本后解密。
type Keyring struct {
	keys     map[string]cipher.AEAD
	active   string
	deferred *ModelEncryption
}

// NewDeferredKeyring 只复制配置；在加解密时验证密钥。
func NewDeferredKeyring(c ModelEncryption) *Keyring {
	copy := ModelEncryption{Keys: make(map[string]string)}
	for version, value := range c.Keys {
		copy.Keys[version] = value
	}
	return &Keyring{deferred: &copy}
}

func NewKeyring(c ModelEncryption) (*Keyring, error) {
	if len(c.Keys) == 0 {
		return nil, invalid("model_encryption.keys", "at least one versioned base64 AES key is required")
	}
	k := &Keyring{keys: make(map[string]cipher.AEAD)}
	var newest uint64
	for version, value := range c.Keys {
		n, err := encryptionVersion(version)
		if err != nil {
			return nil, err
		}
		if n > newest {
			newest, k.active = n, version
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
	return k, nil
}

func (k *Keyring) Encrypt(plaintext string, weight int64) (schema.EncryptedKey, error) {
	if k != nil && k.deferred != nil {
		ring, err := NewKeyring(*k.deferred)
		if err != nil {
			return schema.EncryptedKey{}, err
		}
		return ring.Encrypt(plaintext, weight)
	}
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
	if k != nil && k.deferred != nil {
		ring, err := NewKeyring(ModelEncryption{Keys: map[string]string{key.Version: k.deferred.Keys[key.Version]}})
		if err != nil {
			return "", err
		}
		return ring.Decrypt(key)
	}
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
	seen := make(map[string]bool)
	for _, key := range keys {
		id := StoredKeyID(key)
		if strings.TrimSpace(id) == "" || seen[id] {
			return nil, invalid("model.api_keys.id", "key ids must be nonblank and unique within a model")
		}
		seen[id] = true
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
