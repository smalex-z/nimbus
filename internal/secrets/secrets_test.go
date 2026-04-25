package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatal(err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfoo\n-----END OPENSSH PRIVATE KEY-----\n")
	ct, nonce, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	got, err := c.Decrypt(ct, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestDecryptRejectsTamper(t *testing.T) {
	key := make([]byte, KeyLen)
	c, _ := New(key)
	ct, nonce, _ := c.Encrypt([]byte("hello"))
	ct[0] ^= 0xFF
	if _, err := c.Decrypt(ct, nonce); err == nil {
		t.Fatal("expected decrypt error on tampered ciphertext")
	}
}

func TestNonceUnique(t *testing.T) {
	key := make([]byte, KeyLen)
	c, _ := New(key)
	_, n1, _ := c.Encrypt([]byte("x"))
	_, n2, _ := c.Encrypt([]byte("x"))
	if bytes.Equal(n1, n2) {
		t.Fatal("nonces collided across two calls — would leak plaintext under reuse")
	}
}

func TestLoadOrCreateKey_GeneratesAndPersists(t *testing.T) {
	t.Setenv(EnvVar, "")
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	key, err := LoadOrCreateKey(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeyLen {
		t.Fatalf("key len = %d, want %d", len(key), KeyLen)
	}

	// Env var should now be set; file should contain the same value.
	envVal := os.Getenv(EnvVar)
	decoded, err := base64.StdEncoding.DecodeString(envVal)
	if err != nil {
		t.Fatalf("env var not base64: %v", err)
	}
	if !bytes.Equal(decoded, key) {
		t.Fatal("env var does not match returned key")
	}
	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	want := EnvVar + "=" + envVal + "\n"
	if string(contents) != want {
		t.Fatalf("env file contents = %q, want %q", contents, want)
	}
}

func TestLoadOrCreateKey_ReusesExisting(t *testing.T) {
	raw := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, base64.StdEncoding.EncodeToString(raw))

	key, err := LoadOrCreateKey("")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, raw) {
		t.Fatal("LoadOrCreateKey did not return existing env key")
	}
}
