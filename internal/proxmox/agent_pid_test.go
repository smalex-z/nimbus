package proxmox_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"nimbus/internal/proxmox"
)

// The exact body a live PVE 9 returns when the guest agent no longer
// knows the PID. Captured from a real cluster — note the PID is
// rendered as "ld", an upstream %ld that lost its %, which is why the
// match is on the phrase rather than the number.
const agentPIDGoneBody = `{"data":null,"message":"Agent error: PID ld does not exist\n"}`

func TestAgentExecStatus_PIDGoneNormalizes(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(agentPIDGoneBody))
	})

	_, err := c.AgentExecStatus(context.Background(), "node1", 106, 1302)
	if !errors.Is(err, proxmox.ErrAgentPIDGone) {
		t.Fatalf("expected ErrAgentPIDGone, got %T %v", err, err)
	}
	// The PID we asked about is the useful one — Proxmox's own message
	// can't supply it, so the wrapper must.
	if !strings.Contains(err.Error(), "1302") {
		t.Errorf("error should name the PID we polled, got: %v", err)
	}
}

// An unrelated 500 must NOT be swallowed as a vanished PID.
func TestAgentExecStatus_OtherErrorNotMisread(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"data":null,"message":"Agent error: command timed out\n"}`))
	})

	_, err := c.AgentExecStatus(context.Background(), "node1", 106, 1302)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, proxmox.ErrAgentPIDGone) {
		t.Errorf("unrelated 500 misclassified as ErrAgentPIDGone: %v", err)
	}
}

// AgentRun must surface the vanished PID as such rather than silently
// re-running a command whose outcome it can't determine.
func TestAgentRun_PIDGoneSurfacesWithoutReExec(t *testing.T) {
	t.Parallel()
	var execs int
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/exec") {
			execs++
			writeEnvelope(w, map[string]any{"pid": 1302})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(agentPIDGoneBody))
	})

	_, err := c.AgentRun(context.Background(), "node1", 106, []string{"/bin/sh"}, "echo hi", 10*time.Millisecond)
	if !errors.Is(err, proxmox.ErrAgentPIDGone) {
		t.Fatalf("expected ErrAgentPIDGone, got %T %v", err, err)
	}
	if execs != 1 {
		t.Errorf("command was submitted %d times; a non-idempotent bootstrap must not be retried blindly", execs)
	}
}
