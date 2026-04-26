package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"nimbus/internal/config"
	"nimbus/internal/ctxutil"
	"nimbus/internal/service"
)

// TestAdminRoutesRejectAnonymous verifies that every admin-gated route
// returns 401 when called without a session cookie. This catches the most
// likely regression: a route accidentally registered outside the requireAuth
// group, where it would return 2xx (or 5xx from nil-deref) instead of 401.
//
// We pass nil for most Deps fields. requireAuth's anonymous branch returns 401
// before touching the AuthService, so a bare router is enough to exercise
// the middleware ordering.
func TestAdminRoutesRejectAnonymous(t *testing.T) {
	t.Parallel()

	h := NewRouter(Deps{
		Config: &config.Config{},
	})

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/nodes"},
		{http.MethodGet, "/api/ips"},
		{http.MethodPost, "/api/ips/reconcile"},
		{http.MethodGet, "/api/cluster/vms"},
		{http.MethodDelete, "/api/cluster/vms/1"},
		{http.MethodGet, "/api/cluster/stats"},
		{http.MethodGet, "/api/admin/bootstrap-status"},
		{http.MethodPost, "/api/admin/bootstrap-templates"},
		{http.MethodGet, "/api/settings/oauth"},
		{http.MethodPut, "/api/settings/oauth"},
		{http.MethodDelete, "/api/vms/1"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401 (admin route reachable without auth)", rec.Code)
			}
		})
	}
}

// TestRequireAdmin checks the role gate's three relevant cases: missing user
// in context, non-admin user, and admin user.
func TestRequireAdmin(t *testing.T) {
	t.Parallel()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	gated := requireAdmin(next)

	cases := []struct {
		name       string
		ctx        context.Context
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "no user in context",
			ctx:        context.Background(),
			wantStatus: http.StatusForbidden,
			wantCalled: false,
		},
		{
			name:       "non-admin user",
			ctx:        ctxutil.WithUser(context.Background(), &service.UserView{ID: 1, IsAdmin: false}),
			wantStatus: http.StatusForbidden,
			wantCalled: false,
		},
		{
			name:       "admin user",
			ctx:        ctxutil.WithUser(context.Background(), &service.UserView{ID: 2, IsAdmin: true}),
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tc.ctx)
			rec := httptest.NewRecorder()
			gated.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Errorf("next called: got %v, want %v", called, tc.wantCalled)
			}
		})
	}
}
