package manager

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CryptoHelper encrypts the secrets of the connection profiles (the Oracle
// password and the agent token) with a key kept on this machine.
type CryptoHelper struct {
	key []byte
}

// NewCryptoHelper loads the 32-byte key at keyPath. When the file does not
// exist yet, it generates a key from the system's secure random source and
// writes it there, readable and writable by the owner only.
func NewCryptoHelper(keyPath string) (*CryptoHelper, error) {
	if keyPath == "" {
		keyPath = ".agent.key"
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read agent key file: %w", err)
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate random key: %w", err)
		}
		if dir := filepath.Dir(keyPath); dir != "." {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, fmt.Errorf("create key directory: %w", err)
			}
		}
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			return nil, fmt.Errorf("save agent key file: %w", err)
		}
	}

	if len(key) != 32 {
		return nil, fmt.Errorf("invalid agent key length: expected 32 bytes, got %d", len(key))
	}
	return &CryptoHelper{key: key}, nil
}

// aead returns AES-256-GCM under the agent's key.
func (c *CryptoHelper) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("create cipher block: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return gcm, nil
}

// Encrypt returns plainText encrypted with a random nonce, the nonce first,
// as base64. An empty string stays empty.
func (c *CryptoHelper) Encrypt(plainText string) (string, error) {
	if plainText == "" {
		return "", nil
	}
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plainText), nil)), nil
}

// Decrypt reverses Encrypt. It fails when the value was encrypted under
// another key or has been altered.
func (c *CryptoHelper) Decrypt(encodedCipherText string) (string, error) {
	if encodedCipherText == "" {
		return "", nil
	}
	cipherText, err := base64.StdEncoding.DecodeString(encodedCipherText)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	if len(cipherText) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, sealed := cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():]
	plainText, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w (was the key file replaced?)", err)
	}
	return string(plainText), nil
}
