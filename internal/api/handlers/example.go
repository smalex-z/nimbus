package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"nimbus/internal/api/response"
	"nimbus/internal/service"

	"github.com/go-chi/chi/v5"
)

// Example provides CRUD handlers for the example User resource.
type Example struct {
	svc *service.ExampleService
}

// NewExample creates a new Example handler.
func NewExample(svc *service.ExampleService) *Example {
	return &Example{svc: svc}
}

type createUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ListUsers handles GET /api/users.
func (e *Example) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := e.svc.ListUsers()
	if err != nil {
		response.InternalError(w, "internal server error")
		return
	}
	response.Success(w, users)
}

// CreateUser handles POST /api/users.
func (e *Example) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid request body")
		return
	}
	if req.Name == "" || req.Email == "" {
		response.BadRequest(w, "name and email are required")
		return
	}

	user, err := e.svc.CreateUser(req.Name, req.Email)
	if err != nil {
		response.InternalError(w, "internal server error")
		return
	}
	response.Created(w, user)
}

// DeleteUser handles DELETE /api/users/{id}.
func (e *Example) DeleteUser(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	if err := e.svc.DeleteUser(uint(id)); err != nil {
		response.InternalError(w, "internal server error")
		return
	}
	response.NoContent(w)
}
