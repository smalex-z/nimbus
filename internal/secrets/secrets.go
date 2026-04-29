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
	"strings"
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

// LoadOrCreateKey resolves the master key. Resolution order:
//
//  1. Process env (NIMBUS_ENCRYPTION_KEY already set — typical when systemd
//     loaded the EnvironmentFile before exec).
//  2. The first NIMBUS_ENCRYPTION_KEY=… line in envFilePath. Falling through
//     to (3) without consulting the file would generate a NEW key and
//     append it, which silently rotates the at-rest encryption key out
//     from under every encrypted SSH blob in the DB. Reading existing
//     entries first keeps manual `nimbus` invocations (no systemd) safe.
//  3. Generate a fresh 32-byte key and persist it to envFilePath.
//
// As a side effect of (2), if the file contains *multiple*
// NIMBUS_ENCRYPTION_KEY lines the file is rewritten to keep only the first
// one. Earlier versions of this function appended on every cold start that
// didn't see the env var pre-loaded, so production env files have grown
// duplicate keys over time — the first is the canonical key (matches what
// systemd's EnvironmentFile resolves to and what already encrypted the
// vault); later lines are dead weight that would shift the resolved key
// the day systemd's dedupe behaviour changes.
//
// envFilePath may be empty (tests / one-shot CLI runs); then both the
// file-read fallback and persistence are skipped.
func LoadOrCreateKey(envFilePath string) ([]byte, error) {
	if v := os.Getenv(EnvVar); v != "" {
		return decodeKey(v)
	}

	if envFilePath != "" {
		v, dupes, err := readFirstEnvValue(envFilePath, EnvVar)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", envFilePath, err)
		}
		if v != "" {
			if dupes > 0 {
				if err := dedupeEnvKey(envFilePath, EnvVar); err != nil {
					// Dedupe failure isn't fatal — we still got a usable
					// key; just log via a returned wrapped error so the
					// caller can decide.
					return nil, fmt.Errorf("dedupe %s in %s: %w", EnvVar, envFilePath, err)
				}
			}
			if err := os.Setenv(EnvVar, v); err != nil {
				return nil, fmt.Errorf("set %s: %w", EnvVar, err)
			}
			return decodeKey(v)
		}
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

// decodeKey base64-decodes a key string and validates length.
func decodeKey(v string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", EnvVar, err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%s decodes to %d bytes, want %d", EnvVar, len(key), KeyLen)
	}
	return key, nil
}

// readFirstEnvValue returns the value of the first KEY=… line in path, plus
// the count of additional duplicate lines. Empty value + zero error +
// dupes==0 means the key isn't in the file. A non-existent file is
// reported as empty + nil err, not an error — callers fall through to the
// generate path.
func readFirstEnvValue(path, key string) (string, int, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	prefix := key + "="
	first := ""
	dupes := 0
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		if first == "" {
			first = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
			continue
		}
		dupes++
	}
	return first, dupes, nil
}

// dedupeEnvKey rewrites path keeping only the first KEY=… line for the
// given key, preserving every other line verbatim. Atomic via tempfile +
// rename so a crash mid-write can't corrupt the env file. Permissions are
// preserved from the original (defaults to 0600 if stat fails).
func dedupeEnvKey(path, key string) error {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	prefix := key + "="
	var out strings.Builder
	seen := false
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			if seen {
				continue // drop dup
			}
			seen = true
		}
		out.WriteString(line)
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	tmp, err := os.CreateTemp(deriveDir(path), "nimbus-env-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(out.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// deriveDir returns the directory containing path. Used to keep the
// dedupe tempfile on the same filesystem as the target so the rename is
// atomic.
func deriveDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
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
