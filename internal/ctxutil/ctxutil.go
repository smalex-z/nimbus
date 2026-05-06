// Package ctxutil provides request-scoped context helpers shared between
// the api and handlers packages.
package ctxutil

import (
	"context"

	"nimbus/internal/service"
)

type key int

const (
	userKey key = iota
	clientIPKey
	requestIDKey
)

// WithUser attaches a UserView to the context.
func WithUser(ctx context.Context, u *service.UserView) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// User retrieves the UserView from the context. Returns nil if not set.
func User(ctx context.Context) *service.UserView {
	u, _ := ctx.Value(userKey).(*service.UserView)
	return u
}

// WithClientIP attaches the request's source IP to the context. Set by
// the audit middleware so service-layer code can stamp it on an
// AuditEvent without threading the *http.Request through every
// signature. Empty for non-HTTP code paths (background reconcile, CLI).
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// ClientIP retrieves the client IP attached by the audit middleware.
// Empty when not set; audit readers render that as "—".
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// WithRequestID attaches chi's per-request id to the context. Lifted
// out of chi's own middleware key so packages that emit audit events
// don't have to import chi just to read it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID retrieves the per-request id attached by the audit
// middleware. Empty for non-HTTP code paths.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
