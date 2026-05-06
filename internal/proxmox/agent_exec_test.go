package proxmox_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestClient_AgentExec_Wire asserts the POST /agent/exec call shape.
// Multi-arg commands must hit the wire as repeated `command=` form
// values — Proxmox's perl-side parser interprets that as an array.
// Using a single comma-joined value would silently mis-parse.
func TestClient_AgentExec_Wire(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath, capturedCT, capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, map[string]any{"pid": 1234})
	})

	pid, err := c.AgentExec(context.Background(), "alpha", 200, []string{"/bin/sh", "-c", "echo hi"}, "")
	if err != nil {
		t.Fatalf("AgentExec: %v", err)
	}
	if pid != 1234 {
		t.Errorf("pid = %d, want 1234", pid)
	}
	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/alpha/qemu/200/agent/exec" {
		t.Errorf("path = %q", capturedPath)
	}
	if capturedCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", capturedCT)
	}
	vals, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	got := vals["command"]
	want := []string{"/bin/sh", "-c", "echo hi"}
	if len(got) != len(want) {
		t.Fatalf("command values = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestClient_AgentExec_InputData asserts input-data is form-encoded
// alongside the command. Used by the bootstrap paths to feed multi-line
// shell scripts via stdin without escape-quoting the whole thing into
// a -c argument.
func TestClient_AgentExec_InputData(t *testing.T) {
	t.Parallel()
	var capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, map[string]any{"pid": 1})
	})
	if _, err := c.AgentExec(context.Background(), "n", 1, []string{"/bin/sh"}, "echo hi\nexit 0\n"); err != nil {
		t.Fatalf("AgentExec: %v", err)
	}
	vals, _ := url.ParseQuery(capturedBody)
	if got := vals.Get("input-data"); got != "echo hi\nexit 0\n" {
		t.Errorf("input-data = %q", got)
	}
}

// TestClient_AgentExec_RejectsEmpty asserts the defensive guard fires
// before the HTTP call — empty command is meaningless and Proxmox
// would reject it with a less-helpful error.
func TestClient_AgentExec_RejectsEmpty(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeEnvelope(w, nil)
	})
	if _, err := c.AgentExec(context.Background(), "n", 1, nil, ""); err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times — guard should reject before HTTP", calls.Load())
	}
}

// TestClient_AgentExecStatus_Wire asserts pid is sent as a query param
// on the GET (Proxmox's exec-status endpoint reads pid from query, not
// body — c.do places url.Values in the URL for GETs).
func TestClient_AgentExecStatus_Wire(t *testing.T) {
	t.Parallel()
	var capturedQuery string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		writeEnvelope(w, map[string]any{
			"exited":   1,
			"exitcode": 0,
			"out-data": "hi\n",
		})
	})
	st, err := c.AgentExecStatus(context.Background(), "alpha", 200, 4242)
	if err != nil {
		t.Fatalf("AgentExecStatus: %v", err)
	}
	if st.Exited != 1 || st.ExitCode != 0 || st.OutData != "hi\n" {
		t.Errorf("decoded wrong: %+v", st)
	}
	if !strings.Contains(capturedQuery, "pid=4242") {
		t.Errorf("query = %q, expected pid=4242", capturedQuery)
	}
}

// TestClient_AgentRun_PollsUntilExited covers the full submit+poll
// loop. Until the third status call, exited=0 (still running); the
// helper must keep ticking. The real bootstrap paths depend on this
// to capture exit codes — without it they'd false-success on a
// still-running command.
func TestClient_AgentRun_PollsUntilExited(t *testing.T) {
	t.Parallel()
	var statusCalls atomic.Int32
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/exec") {
			writeEnvelope(w, map[string]any{"pid": 9})
			return
		}
		// exec-status — return exited=0 twice, then exited=1.
		n := statusCalls.Add(1)
		if n < 3 {
			writeEnvelope(w, map[string]any{"exited": 0})
			return
		}
		writeEnvelope(w, map[string]any{
			"exited":   1,
			"exitcode": 0,
			"out-data": "done\n",
		})
	})

	st, err := c.AgentRun(context.Background(), "n", 1, []string{"/bin/true"}, "", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("AgentRun: %v", err)
	}
	if st.Exited != 1 || st.ExitCode != 0 || st.OutData != "done\n" {
		t.Errorf("final status = %+v", st)
	}
	if got := statusCalls.Load(); got < 3 {
		t.Errorf("status calls = %d, expected >= 3", got)
	}
}
