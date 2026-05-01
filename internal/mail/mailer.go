// Package mail is a focused SMTP client for outbound transactional
// email. Today the only callers are the admin-side "send test email"
// affordance on /email and the magic-link recovery flow that emails
// password-only users when the workspace is moving to OAuth-only
// sign-in.
//
// All sends are synchronous and one-shot — there's no queue, no
// background worker, no retry. Volume is low enough that a request
// can wait on the SMTP roundtrip; the caller is expected to set a
// context deadline appropriate for their use case (10–30s).
//
// Encryption modes match the values stored on db.SMTPSettings:
//   - "tls"      — direct TLS dial, port 465 / SMTPS.
//   - "starttls" — plain dial then STARTTLS, port 587 / submission.
//   - "none"     — plain unencrypted SMTP. Only safe inside trusted
//     networks; net/smtp's PlainAuth refuses to run
//     over non-TLS unless the server is localhost.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"sort"
	"strconv"
	"strings"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/secrets"
)

// Encryption is the wire mode this mailer uses to dial the server.
// String values match the values persisted on db.SMTPSettings.
type Encryption string

const (
	EncryptionStartTLS Encryption = "starttls"
	EncryptionTLS      Encryption = "tls"
	EncryptionNone     Encryption = "none"
)

// ErrNotConfigured is returned by Resolve when host or from-address are
// blank — the bare minimum to attempt a send. The caller is expected
// to surface this as a 4xx so the admin understands they need to
// finish the /email form first.
var ErrNotConfigured = errors.New("smtp not configured")

// ErrCipherUnavailable is returned by Resolve when the SMTP row has a
// non-empty password ciphertext but the cipher needed to decrypt it
// wasn't supplied. Distinct from ErrNotConfigured because the admin's
// remediation is different: this means the server didn't load
// NIMBUS_ENCRYPTION_KEY at startup, not that the form is empty.
var ErrCipherUnavailable = errors.New("encryption cipher unavailable; cannot decrypt smtp password")

// Config is the resolved (decrypted-password) shape passed to Send.
// Distinct from db.SMTPSettings because that struct holds ciphertext;
// nothing outside this package decrypts.
type Config struct {
	Host       string
	Port       int
	Username   string
	Password   string
	From       string
	Encryption Encryption
}

// Message is a single plain-text email. We don't ship HTML in v1 —
// magic-link recovery is short enough that a hyperlink rendered as a
// URL on its own line is fine, and HTML email comes with multipart
// + content-encoding fiddly bits we don't need yet.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Resolve decrypts the SMTP password ciphertext and returns a Config
// ready to pass to Send. Returns ErrNotConfigured when the form is
// empty, ErrCipherUnavailable when the password is set but no cipher
// was supplied. Defaults port to 587 and encryption to starttls when
// the row left them blank — matches what the /email form does on save.
func Resolve(s *db.SMTPSettings, cipher *secrets.Cipher) (*Config, error) {
	if s == nil || s.Host == "" || s.FromAddress == "" {
		return nil, ErrNotConfigured
	}
	cfg := &Config{
		Host:       s.Host,
		Port:       s.Port,
		Username:   s.Username,
		From:       s.FromAddress,
		Encryption: Encryption(s.Encryption),
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Encryption == "" {
		cfg.Encryption = EncryptionStartTLS
	}
	if len(s.PasswordCT) > 0 {
		if cipher == nil {
			return nil, ErrCipherUnavailable
		}
		plain, err := cipher.Decrypt(s.PasswordCT, s.PasswordNonce)
		if err != nil {
			return nil, fmt.Errorf("decrypt smtp password: %w", err)
		}
		cfg.Password = string(plain)
	}
	return cfg, nil
}

// Send dials the SMTP server, authenticates if credentials are set,
// and delivers msg. The TLS strategy is picked from cfg.Encryption.
func Send(ctx context.Context, cfg *Config, msg Message) error {
	if cfg == nil {
		return errors.New("smtp: nil config")
	}
	if msg.To == "" {
		return errors.New("smtp: empty recipient")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	c, err := dialClient(ctx, cfg, addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp RCPT TO %q: %w", msg.To, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write([]byte(BuildMessage(cfg.From, msg))); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp body write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp body close: %w", err)
	}
	return c.Quit()
}

// dialClient handles the three encryption modes and returns a ready
// *smtp.Client. tls and starttls share a server-name check so a
// misconfigured DNS / cert mismatch surfaces as a TLS error rather
// than a silent downgrade.
func dialClient(ctx context.Context, cfg *Config, addr string) (*smtp.Client, error) {
	switch cfg.Encryption {
	case EncryptionTLS:
		d := &tls.Dialer{Config: &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("smtp tls dial %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("smtp client init: %w", err)
		}
		return c, nil
	case EncryptionNone, EncryptionStartTLS, "":
		d := &net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("smtp dial %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("smtp client init: %w", err)
		}
		if cfg.Encryption == EncryptionStartTLS || cfg.Encryption == "" {
			if err := c.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("smtp starttls: %w", err)
			}
		}
		return c, nil
	default:
		return nil, fmt.Errorf("smtp: unknown encryption mode %q", cfg.Encryption)
	}
}

// BuildMessage formats msg as RFC 5322 plain-text bytes ready to feed
// to smtp.Client.Data. Exported so handlers can preview the body in
// logs / dry-run paths without dialing the server. Headers are
// sorted alphabetically — order is irrelevant to receivers but a
// stable ordering makes the unit test deterministic.
func BuildMessage(from string, msg Message) string {
	headers := map[string]string{
		"From":                      from,
		"To":                        msg.To,
		"Subject":                   msg.Subject,
		"Date":                      time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version":              "1.0",
		"Content-Type":              "text/plain; charset=UTF-8",
		"Content-Transfer-Encoding": "8bit",
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(headers[k])
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	return b.String()
}
