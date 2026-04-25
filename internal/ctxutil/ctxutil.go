// Package ctxutil provides request-scoped context helpers shared between
// the api and handlers packages.
package ctxutil

import (
	"context"

	"nimbus/internal/service"
)

type key int

const userKey key = iota

// WithUser attaches a UserView to the context.
func WithUser(ctx context.Context, u *service.UserView) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// User retrieves the UserView from the context. Returns nil if not set.
func User(ctx context.Context) *service.UserView {
	u, _ := ctx.Value(userKey).(*service.UserView)
	return u
}
