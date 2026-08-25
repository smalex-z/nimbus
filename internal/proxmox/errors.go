package proxmox

import (
	"errors"
	"fmt"
)

// ErrNotFound indicates a 404 from the Proxmox API. Used by TemplateExists
// to distinguish "missing" from "transient error".
var ErrNotFound = errors.New("proxmox: not found")

// ErrAgentPIDGone means exec-status was asked about a PID the in-guest
// qemu-guest-agent no longer knows. It does NOT mean the command failed —
// it means we lost the channel we were watching it on.
//
// Two ways to get here, and neither is a bug in the caller:
//
//   - The agent restarted. Its guest-exec table is in-memory only, so a
//     restart forgets every PID. Worse, the unit ships
//     KillMode=control-group, so the restart also killed the command
//     itself — guest-exec children live in the unit's cgroup. A
//     first-boot `apt dist-upgrade` that includes the qemu-guest-agent
//     package does exactly this, which is why provision waits for
//     cloud-init before running any agent-exec bootstrap.
//   - The status was already read once. The agent frees the entry after
//     the first terminal read, so a second poll for the same PID 404s
//     (as a 500, see below).
//
// Proxmox reports it as HTTP 500 with an "Agent error: PID ... does not
// exist" body — the same normalize-500-to-a-real-error situation
// GetVMConfig has. The PID in that message is literal garbage ("PID ld"):
// a %ld that lost its %, upstream. Don't try to parse it.
var ErrAgentPIDGone = errors.New("proxmox: guest-exec PID is gone (agent restarted or status already read)")

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
