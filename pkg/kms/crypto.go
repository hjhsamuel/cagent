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

const KeyLength = 32

func Encrypt(key []byte, plaintext string) (*EncryptedValue, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("key must be %d bytes", KeyLength)
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

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return &EncryptedValue{
		Ciphertext: ciphertext,
		Nonce:      nonce,
	}, nil
}

func Decrypt(key, ciphertext, nonce []byte) (string, error) {
	if len(key) != KeyLength {
		return "", fmt.Errorf("key must be %d bytes", KeyLength)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
