package api

import (
	"net/http"

	"nimbus/internal/api/handlers"
	"nimbus/internal/oauth"
	"nimbus/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds and returns the application router.
func NewRouter(svc *service.ExampleService, authSvc *service.AuthService, github, google oauth.Provider) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(loggingMiddleware)
	r.Use(recoveryMiddleware)
	r.Use(rateLimiter(100, 200))

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", handlers.Health)

		// Public auth routes
		auth := handlers.NewAuth(authSvc, github, google)
		r.Post("/auth/register", auth.Register)
		r.Post("/auth/login", auth.Login)
		r.Post("/auth/logout", auth.Logout)
		r.Get("/auth/github", auth.GitHubStart)
		r.Get("/auth/github/callback", auth.GitHubCallback)
		r.Get("/auth/google", auth.GoogleStart)
		r.Get("/auth/google/callback", auth.GoogleCallback)

		// Public admin status — needed by frontend before login
		admin := handlers.NewAdmin(authSvc)
		r.Get("/admin/status", admin.Status)

		// Protected routes — require a valid session cookie
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(authSvc))

			r.Get("/me", auth.Me)
			r.Get("/users", auth.ListUsers)
			r.Post("/admin/claim", admin.Claim)
		})
	})

	return r
}
