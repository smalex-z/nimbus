package mail_test

import (
	"errors"
	"strings"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/mail"
	"nimbus/internal/secrets"
)

// BuildMessage's exact byte layout is what hits the SMTP server's
// DATA command; getting it wrong (missing CRLFs, missing required
// headers, Subject buried in the body) results in mail that providers
// reject or quarantine. This test pins the contract.
func TestBuildMessage_ContainsRequiredHeaders(t *testing.T) {
	t.Parallel()
	out := mail.BuildMessage("nimbus@example.com", mail.Message{
		To:      "user@example.com",
		Subject: "hello",
		Body:    "hi there\r\n",
	})

	for _, want := range []string{
		"From: nimbus@example.com\r\n",
		"To: user@example.com\r\n",
		"Subject: hello\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"Content-Transfer-Encoding: 8bit\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}

	// Body separator: blank line (CRLF) between headers and body.
	if !strings.Contains(out, "\r\n\r\nhi there") {
		t.Errorf("missing CRLF separator before body:\n%s", out)
	}
}

// Resolve refuses to hand back a Config when the form is empty. The
// /email page's "Send test email" button gates on `view.configured`
// to avoid hitting this; the path exists so a hand-crafted curl can't
// trigger a dial against a blank host.
func TestResolve_ErrNotConfiguredOnEmpty(t *testing.T) {
	t.Parallel()
	_, err := mail.Resolve(&db.SMTPSettings{}, nil)
	if !errors.Is(err, mail.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// Cipher is required when password ciphertext is present. The auth
// service guards against this at write time too (saving with a
// password requires a cipher), but a server that lost its env file
// post-write could still hit this on the read path.
func TestResolve_ErrCipherUnavailableWhenPasswordSet(t *testing.T) {
	t.Parallel()
	row := &db.SMTPSettings{
		Host:        "smtp.example.com",
		FromAddress: "nimbus@example.com",
		PasswordCT:  []byte("ciphertext-bytes"),
	}
	_, err := mail.Resolve(row, nil)
	if !errors.Is(err, mail.ErrCipherUnavailable) {
		t.Fatalf("err = %v, want ErrCipherUnavailable", err)
	}
}

// Resolve fills in the documented defaults: port 587 + starttls when
// the row left them blank. Matches the /email form's defaults so the
// admin's mental model lines up with what's actually used to dial.
func TestResolve_AppliesDefaults(t *testing.T) {
	t.Parallel()
	row := &db.SMTPSettings{Host: "smtp.example.com", FromAddress: "nimbus@example.com"}
	cfg, err := mail.Resolve(row, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Port != 587 {
		t.Errorf("port = %d, want 587 default", cfg.Port)
	}
	if cfg.Encryption != mail.EncryptionStartTLS {
		t.Errorf("encryption = %q, want %q default", cfg.Encryption, mail.EncryptionStartTLS)
	}
}

// Resolve actually decrypts when both the cipher and ciphertext are
// supplied — confirms the round-trip across the secrets layer works
// in the same shape /email's save path stores it.
func TestResolve_DecryptsPassword(t *testing.T) {
	t.Parallel()
	c, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	ct, nonce, err := c.Encrypt([]byte("hunter2"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	row := &db.SMTPSettings{
		Host: "smtp.example.com", FromAddress: "nimbus@example.com",
		PasswordCT: ct, PasswordNonce: nonce,
	}
	cfg, err := mail.Resolve(row, c)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Password != "hunter2" {
		t.Errorf("password = %q, want %q", cfg.Password, "hunter2")
	}
}
