package kms

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	for _, size := range []int{KeyLength} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			key := bytes.Repeat([]byte{1}, size)
			for _, plaintext := range []string{"", "api-secret", "中文密钥"} {
				encrypted, err := Encrypt(key, plaintext)
				if err != nil {
					t.Fatal(err)
				}
				if len(encrypted.Nonce) != NonceSize || len(encrypted.Ciphertext) != len(plaintext)+TagSize {
					t.Fatal("nonce must be stored separately from ciphertext")
				}
				got, err := Decrypt(key, encrypted.Ciphertext, encrypted.Nonce)
				if err != nil || got != plaintext {
					t.Fatal("round trip failed", err)
				}
			}
		})
	}
}

func TestDecryptKnownAESGCMVector(t *testing.T) {
	// NIST AES-GCM vector: a zero AES-256 key, nonce and 16-byte plaintext.
	ciphertext, err := hex.DecodeString("cea7403d4d606b6e074ec5d3baf39d18d0d1c8a799996bf0265b98b5d48ab919")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := Decrypt(make([]byte, KeyLength), ciphertext, make([]byte, NonceSize))
	if err != nil || plaintext != string(make([]byte, 16)) {
		t.Fatal("known AES-GCM vector could not be decrypted", err)
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{1}, KeyLength)
	encrypted, err := EncryptWithAAD(key, "api-secret", []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecryptWithAAD(key, encrypted.Ciphertext, encrypted.Nonce, []byte("v1")); err != nil || got != "api-secret" {
		t.Fatal("authenticated round trip failed", err)
	}
	damaged := bytes.Clone(encrypted.Ciphertext)
	damaged[0] ^= 1
	wrongNonce := bytes.Clone(encrypted.Nonce)
	wrongNonce[0] ^= 1
	for _, tc := range []struct {
		name                        string
		key, ciphertext, nonce, aad []byte
	}{
		{"wrong key", bytes.Repeat([]byte{2}, KeyLength), encrypted.Ciphertext, encrypted.Nonce, []byte("v1")},
		{"wrong version", key, encrypted.Ciphertext, encrypted.Nonce, []byte("v2")},
		{"missing AAD", key, encrypted.Ciphertext, encrypted.Nonce, nil},
		{"damaged ciphertext", key, damaged, encrypted.Nonce, []byte("v1")},
		{"truncated ciphertext", key, encrypted.Ciphertext[:TagSize-1], encrypted.Nonce, []byte("v1")},
		{"missing ciphertext", key, nil, encrypted.Nonce, []byte("v1")},
		{"wrong nonce", key, encrypted.Ciphertext, wrongNonce, []byte("v1")},
		{"short nonce", key, encrypted.Ciphertext, encrypted.Nonce[:NonceSize-1], []byte("v1")},
		{"long nonce", key, encrypted.Ciphertext, make([]byte, NonceSize+1), []byte("v1")},
		{"missing nonce", key, encrypted.Ciphertext, nil, []byte("v1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if plaintext, err := DecryptWithAAD(tc.key, tc.ciphertext, tc.nonce, tc.aad); err == nil || plaintext != "" {
				t.Fatal("invalid encrypted value accepted")
			}
		})
	}
}

func TestRejectInvalidKeys(t *testing.T) {
	for _, size := range []int{0, 15, 16, 17, 23, 24, 25, 31, 33} {
		key := make([]byte, size)
		if err := ValidateKey(key); err == nil {
			t.Fatalf("accepted %d-byte key", size)
		}
		if encrypted, err := Encrypt(key, "api-secret"); err == nil || encrypted != nil {
			t.Fatalf("encrypted with %d-byte key", size)
		}
		if plaintext, err := Decrypt(key, nil, nil); err == nil || plaintext != "" {
			t.Fatalf("decrypted with %d-byte key", size)
		}
	}
}
