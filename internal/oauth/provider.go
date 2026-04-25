// Package oauth defines the pluggable OAuth provider interface used by the
// auth system. Each provider (GitHub, Google, …) implements Provider.
package oauth

import "context"

// UserInfo is the normalised identity returned by any OAuth provider after a
// successful code exchange.
type UserInfo struct {
	ProviderID string // provider-specific user ID (for future deduplication)
	Login      string // username / handle shown in the handshake page
	Name       string // display name
	Email      string // primary verified email
}

// Provider is the interface every OAuth provider must implement.
type Provider interface {
	// AuthURL returns the URL to redirect the user to for authorization.
	// state must be a random, per-request value stored in a cookie.
	AuthURL(state string) string

	// Exchange completes the OAuth flow by trading code for user identity.
	Exchange(ctx context.Context, code string) (*UserInfo, error)
}
