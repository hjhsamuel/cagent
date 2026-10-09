package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

type EncryptedValue struct {
	Ciphertext []byte
	Nonce      []byte
}

const (
	KeyLength = 32
	NonceSize = 12
	TagSize   = 16
)

// ValidateKey requires AES-256 key material.
func ValidateKey(key []byte) error {
	if len(key) != KeyLength {
		return fmt.Errorf("key must be %d bytes", KeyLength)
	}
	return nil
}

func Encrypt(key []byte, plaintext string) (*EncryptedValue, error) {
	return EncryptWithAAD(key, plaintext, nil)
}

// EncryptWithAAD authenticates additionalData and stores the nonce separately.
func EncryptWithAAD(key []byte, plaintext string, additionalData []byte) (*EncryptedValue, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), additionalData)
	return &EncryptedValue{
		Ciphertext: ciphertext,
		Nonce:      nonce,
	}, nil
}

func Decrypt(key, ciphertext, nonce []byte) (string, error) {
	return DecryptWithAAD(key, ciphertext, nonce, nil)
}

// DecryptWithAAD requires the same additionalData used during encryption.
func DecryptWithAAD(key, ciphertext, nonce, additionalData []byte) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(nonce) != gcm.NonceSize() {
		return "", fmt.Errorf("nonce must be %d bytes", gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, additionalData)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
