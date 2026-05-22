package proxmox

import (
	"errors"
	"fmt"
	"net/http"
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

// BindingState classifies the outcome of a Proxmox /version probe so callers
// can tell three operationally distinct failures apart: a healthy binding, a
// host that's gone (down / wrong address / no TLS), and a host that answers
// but rejects our token (401/403 — typically the node left the cluster that
// owns the token, or the secret was rotated).
type BindingState string

const (
	BindingOK           BindingState = "ok"
	BindingUnreachable  BindingState = "unreachable"  // transport-level failure: no HTTP response
	BindingUnauthorized BindingState = "unauthorized" // 401/403: host up, token rejected
)

// Classify maps a Version-probe error to a BindingState. A nil error is
// BindingOK; a *HTTPError carrying 401 or 403 is BindingUnauthorized; anything
// else (dial timeouts, connection refused, TLS errors, other HTTP codes) is
// BindingUnreachable — we lump non-auth HTTP codes in with transport failures
// because for binding-health purposes they all mean "this entry node isn't
// usable", and only 401/403 carries the distinct "token rejected" signal.
func Classify(err error) BindingState {
	if err == nil {
		return BindingOK
	}
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden) {
		return BindingUnauthorized
	}
	return BindingUnreachable
}
