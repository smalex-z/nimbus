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
