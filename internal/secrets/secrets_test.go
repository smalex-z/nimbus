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

// TestLoadOrCreateKey_ReadsFromFileWhenEnvMissing covers the systemd-isn't-
// the-only-caller case. A manual `nimbus` invocation outside the systemd
// unit doesn't get NIMBUS_ENCRYPTION_KEY pre-loaded, but the env file may
// already hold the canonical key — falling through to fresh-generate would
// silently rotate the at-rest encryption key. The fix consults the file
// before generating.
func TestLoadOrCreateKey_ReadsFromFileWhenEnvMissing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nimbus.env"

	raw := bytes.Repeat([]byte{0x42}, KeyLen)
	encoded := base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte("NIMBUS_ENCRYPTION_KEY="+encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, "")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(key, raw) {
		t.Fatal("did not return existing key from file")
	}

	// File should still contain exactly one key line — fresh-generate path
	// must not have fired.
	got, _ := os.ReadFile(path)
	if want := "NIMBUS_ENCRYPTION_KEY=" + encoded + "\n"; string(got) != want {
		t.Errorf("file changed unexpectedly:\n got: %q\nwant: %q", string(got), want)
	}
}

// TestLoadOrCreateKey_DedupesFile is the regression for the production env
// file we found with three duplicate NIMBUS_ENCRYPTION_KEY lines. The first
// is canonical (systemd's EnvironmentFile dedupe takes the first
// occurrence, and that's what the existing vault is encrypted under); the
// extras are leftovers from earlier append-on-cold-start cycles. Loading
// must keep the first and drop the rest.
func TestLoadOrCreateKey_DedupesFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nimbus.env"

	first := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, KeyLen))
	second := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, KeyLen))
	third := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, KeyLen))
	contents := "OTHER=keep\n" +
		"NIMBUS_ENCRYPTION_KEY=" + first + "\n" +
		"NIMBUS_ENCRYPTION_KEY=" + second + "\n" +
		"# comment\n" +
		"NIMBUS_ENCRYPTION_KEY=" + third + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, "")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	wantRaw, _ := base64.StdEncoding.DecodeString(first)
	if !bytes.Equal(key, wantRaw) {
		t.Errorf("returned key isn't the first one in the file")
	}

	got, _ := os.ReadFile(path)
	wantContents := "OTHER=keep\n" +
		"NIMBUS_ENCRYPTION_KEY=" + first + "\n" +
		"# comment\n"
	if string(got) != wantContents {
		t.Errorf("dedup wrong:\n got: %q\nwant: %q", string(got), wantContents)
	}
}

// TestLoadOrCreateKey_GeneratesWhenAbsent covers the truly-fresh-install
// case. No env var, no existing file → generate, persist, set env. Same
// flow as before; just guarding against accidental regression.
func TestLoadOrCreateKey_GeneratesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nimbus.env"
	t.Setenv(EnvVar, "")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(key) != KeyLen {
		t.Errorf("len(key) = %d, want %d", len(key), KeyLen)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Contains(got, []byte("NIMBUS_ENCRYPTION_KEY=")) {
		t.Errorf("file did not get the new key persisted: %q", string(got))
	}
}
