// Package secrets provides AES-256-GCM encryption for at-rest secrets such as
// stored SSH private keys.
//
// The 32-byte master key lives in the NIMBUS_ENCRYPTION_KEY env var (base64
// std-encoded). LoadOrCreateKey generates one on first start and appends it to
// the env file so subsequent startups reuse the same key — losing the key
// makes every encrypted blob in the DB unrecoverable.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// EnvVar is the env var name holding the base64-encoded 32-byte key.
	EnvVar = "NIMBUS_ENCRYPTION_KEY"
	// KeyLen is the required raw key length (AES-256).
	KeyLen = 32
)

// ErrNoCiphertext is returned when Decrypt is called with empty inputs.
var ErrNoCiphertext = errors.New("secrets: empty ciphertext")

// Cipher wraps an AES-GCM AEAD with a fixed key.
type Cipher struct {
	aead cipher.AEAD
}

// New returns a Cipher for the supplied 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes init: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm init: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns (ciphertext, nonce). A fresh random nonce is generated for
// each call — callers must persist both halves to decrypt later.
func (c *Cipher) Encrypt(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	ciphertext = c.aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// Decrypt reverses Encrypt. Returns ErrNoCiphertext if either input is empty.
func (c *Cipher) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(nonce) == 0 {
		return nil, ErrNoCiphertext
	}
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}

// LoadOrCreateKey resolves the master key. If the env var is already set it is
// decoded and returned. Otherwise a fresh 32-byte key is generated, written to
// envFilePath as a KEY=VALUE line, and exported into the current process env.
//
// envFilePath may be empty in which case the generated key is only set in the
// process environment (useful for tests / one-shot CLI runs).
func LoadOrCreateKey(envFilePath string) ([]byte, error) {
	if v := os.Getenv(EnvVar); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", EnvVar, err)
		}
		if len(key) != KeyLen {
			return nil, fmt.Errorf("%s decodes to %d bytes, want %d", EnvVar, len(key), KeyLen)
		}
		return key, nil
	}

	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.Setenv(EnvVar, encoded); err != nil {
		return nil, fmt.Errorf("set %s: %w", EnvVar, err)
	}
	if envFilePath != "" {
		if err := appendEnvLine(envFilePath, EnvVar, encoded); err != nil {
			return nil, fmt.Errorf("persist key to %s: %w", envFilePath, err)
		}
	}
	return key, nil
}

// appendEnvLine appends `KEY=VALUE\n` to path, creating the file with 0600 if
// it does not exist. Existing content is preserved.
func appendEnvLine(path, key, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	_, err = fmt.Fprintf(f, "%s=%s\n", key, value)
	return err
}
