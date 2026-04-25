package proxmox

import (
	"errors"
	"fmt"
)

// ErrNotFound indicates a 404 from the Proxmox API. Used by TemplateExists
// to distinguish "missing" from "transient error".
var ErrNotFound = errors.New("proxmox: not found")

// HTTPError carries the raw status and body of an unexpected Proxmox response.
// Useful for surfacing real failure reasons up to the user instead of "500
// internal server error".
type HTTPError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("proxmox: %s %s returned %d: %s", e.Method, e.Path, e.Status, e.Body)
}
