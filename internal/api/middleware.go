package api

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/service"

	"golang.org/x/time/rate"
)

// responseWriter wraps http.ResponseWriter to capture the status code for
// logging. It also implements http.Hijacker (WebSocket upgrades) and
// http.Flusher (SSE / streaming responses).
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "300")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

// auditContextMiddleware lifts the client IP + chi's per-request id
// into the request context so the audit Service can stamp them on
// recorded events without threading the *http.Request through every
// service-layer signature.
//
// IP extraction prefers `X-Forwarded-For`'s first hop when set
// (Nimbus sits behind Caddy / nginx in real deployments); otherwise
// falls back to RemoteAddr. Caller-supplied X-Forwarded-For values
// are honoured because Nimbus is meant to run behind a trusted
// reverse proxy — a pure-internet deployment without one would want
// stricter trust handling, but that's not the supported topology.
//
// Mounts after middleware.RequestID (which sets the header chi reads)
// and before any handler that might call audit.Record.
func auditContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if reqID := r.Header.Get("X-Request-Id"); reqID != "" {
			ctx = ctxutil.WithRequestID(ctx, reqID)
		}
		ctx = ctxutil.WithClientIP(ctx, clientIPFromRequest(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientIPFromRequest extracts the source IP for audit purposes.
// X-Forwarded-For wins (reverse-proxy deployments); otherwise the
// raw RemoteAddr's host portion. Returns empty string on parse
// failures rather than the literal "unknown" so audit readers can
// render the empty case as "—".
func clientIPFromRequest(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First hop in the chain is the original client.
		if idx := indexByte(xff, ','); idx >= 0 {
			return trimSpace(xff[:idx])
		}
		return trimSpace(xff)
	}
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// trimSpace + indexByte avoid pulling in the `strings` package for a
// two-call hot path. The alternative is `strings.TrimSpace` /
// `strings.IndexByte` — same result, slightly more imports.
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v\n%s", rec, debug.Stack())
				http.Error(w, `{"success":false,"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireAuth validates the session cookie and attaches the UserView to the
// request context. Responds 401 if the cookie is missing or the session is expired.
func requireAuth(authSvc *service.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("nimbus_sid")
			if err != nil {
				response.Error(w, http.StatusUnauthorized, "Not authenticated")
				return
			}

			user, err := authSvc.GetUserBySessionID(cookie.Value)
			if err != nil {
				response.Error(w, http.StatusUnauthorized, "Session expired")
				return
			}

			next.ServeHTTP(w, r.WithContext(ctxutil.WithUser(r.Context(), user)))
		})
	}
}

// requireGPUWorkerToken authenticates the GX10 worker via a pre-shared
// bearer token in the `Authorization: Bearer <hex>` header. Constant-time
// comparison via AuthService.VerifyGPUWorkerToken so timing attacks can't
// recover the token byte-by-byte.
//
// Failure is a flat 401 — no body, no hint about what went wrong.
func requireGPUWorkerToken(authSvc *service.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
				response.Error(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			token := h[len(prefix):]
			if !authSvc.VerifyGPUWorkerToken(token) {
				response.Error(w, http.StatusUnauthorized, "invalid worker token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAdmin responds 403 if the authenticated user is not an admin.
// Must be used after requireAuth.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := ctxutil.User(r.Context())
		if user == nil || !user.IsAdmin {
			response.Error(w, http.StatusForbidden, "Admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireVerified responds 403 with code "access_code_required" when a
// non-admin user has not verified against the current access code version.
// Admins always pass. Must be used after requireAuth.
func requireVerified(authSvc *service.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := ctxutil.User(r.Context())
			if user == nil {
				response.Error(w, http.StatusUnauthorized, "Not authenticated")
				return
			}
			if user.IsAdmin {
				next.ServeHTTP(w, r)
				return
			}
			ok, err := authSvc.IsUserVerified(user.ID)
			if err != nil {
				response.InternalError(w, "failed to check verification")
				return
			}
			if !ok {
				response.Error(w, http.StatusForbidden, "access_code_required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func rateLimiter(rps float64, burst int) func(http.Handler) http.Handler {
	limiter := rate.NewLimiter(rate.Limit(rps), burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow() {
				http.Error(w, `{"success":false,"error":"too many requests"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
