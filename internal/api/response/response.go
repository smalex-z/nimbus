// Package response standardizes JSON responses across the API.
//
// Wire format:
//
//	{"success": true,  "data": ...}                 // success
//	{"success": false, "error": "human message"}    // failure
package response

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	internalerrors "nimbus/internal/errors"
	"nimbus/internal/tunnel"
)

type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

func JSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func Success(w http.ResponseWriter, data interface{}) {
	JSON(w, http.StatusOK, Response{Success: true, Data: data})
}

func Created(w http.ResponseWriter, data interface{}) {
	JSON(w, http.StatusCreated, Response{Success: true, Data: data})
}

func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, Response{Success: false, Error: message})
}

func BadRequest(w http.ResponseWriter, message string) { Error(w, http.StatusBadRequest, message) }
func NotFound(w http.ResponseWriter, message string)   { Error(w, http.StatusNotFound, message) }
func Conflict(w http.ResponseWriter, message string)   { Error(w, http.StatusConflict, message) }
func InternalError(w http.ResponseWriter, message string) {
	Error(w, http.StatusInternalServerError, message)
}
func ServiceUnavailable(w http.ResponseWriter, message string) {
	Error(w, http.StatusServiceUnavailable, message)
}

// FromError maps internal error types to HTTP statuses. Unknown errors are
// logged with full detail and surfaced as 500 with a generic message — never
// leak internal error strings to clients.
//
// Upstream Gopher 4xx responses (carrying actionable messages like "subdomain
// in use" or "URL routing is disabled") were previously falling through to
// the default 500 branch, hiding the actual cause from the SPA. They now
// pass through with the upstream status preserved and Gopher's own error
// string used as the body.
func FromError(w http.ResponseWriter, err error) {
	var (
		validation *internalerrors.ValidationError
		conflict   *internalerrors.ConflictError
		notFound   *internalerrors.NotFoundError
		gopherErr  *tunnel.HTTPError
	)
	switch {
	case errors.As(err, &validation):
		BadRequest(w, validation.Error())
	case errors.As(err, &conflict):
		ServiceUnavailable(w, conflict.Error())
	case errors.As(err, &notFound):
		NotFound(w, notFound.Error())
	case errors.As(err, &gopherErr):
		msg := gopherErr.Body
		if msg == "" {
			msg = http.StatusText(gopherErr.Status)
		}
		switch {
		case gopherErr.Status >= 500:
			// Gopher itself broke — log the upstream detail (might
			// include stack-shaped strings we don't want to leak) and
			// surface a clean 502.
			log.Printf("gopher upstream error: %v", err)
			Error(w, http.StatusBadGateway, "tunnel gateway error: "+http.StatusText(gopherErr.Status))
		default:
			Error(w, gopherErr.Status, msg)
		}
	default:
		log.Printf("internal error: %v", err)
		InternalError(w, "internal server error")
	}
}
