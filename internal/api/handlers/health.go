package handlers

import (
	"net/http"
	"time"

	"homestack/internal/api/response"
)

type healthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// Health handles GET /api/health.
func Health(w http.ResponseWriter, r *http.Request) {
	response.Success(w, healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}
