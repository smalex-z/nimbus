package api

import (
	"net/http"

	"nimbus/internal/api/handlers"
	"nimbus/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds and returns the application router.
func NewRouter(svc *service.ExampleService, authSvc *service.AuthService) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(loggingMiddleware)
	r.Use(recoveryMiddleware)
	r.Use(rateLimiter(100, 200))

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", handlers.Health)

		auth := handlers.NewAuth(authSvc)
		r.Post("/auth/register", auth.Register)

		example := handlers.NewExample(svc)
		r.Get("/users", example.ListUsers)
		r.Post("/users", example.CreateUser)
		r.Delete("/users/{id}", example.DeleteUser)
	})

	return r
}
