package provision_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// stubRunner is a minimal provision.AgentRunner whose reply is decided
// per call, so a test can script the sequence a real first boot
// produces (running → running → done, with failures in between).
type stubRunner struct {
	calls  atomic.Int32
	replyN func(n int) (*proxmox.AgentExecStatus, error)
}

func (s *stubRunner) AgentRun(_ context.Context, _ string, _ int, _ []string, _ string, _ time.Duration) (*proxmox.AgentExecStatus, error) {
	return s.replyN(int(s.calls.Add(1)))
}

func statusOut(out string) (*proxmox.AgentExecStatus, error) {
	return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0, OutData: out}, nil
}

func TestWaitForCloudInit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		replyN    func(n int) (*proxmox.AgentExecStatus, error)
		wantCalls int
	}{
		{
			name:      "already done returns on the first probe",
			replyN:    func(int) (*proxmox.AgentExecStatus, error) { return statusOut("status: done\n") },
			wantCalls: 1,
		},
		{
			name: "waits out running",
			replyN: func(n int) (*proxmox.AgentExecStatus, error) {
				if n < 3 {
					return statusOut("status: running\n")
				}
				return statusOut("status: done\n")
			},
			wantCalls: 3,
		},
		{
			// cloud-init exits non-zero here, and "error" is still a
			// terminal state — it is not this function's job to judge.
			name:      "error is terminal",
			replyN:    func(int) (*proxmox.AgentExecStatus, error) { return statusOut("status: error\n") },
			wantCalls: 1,
		},
		{
			name:      "image without cloud-init proceeds",
			replyN:    func(int) (*proxmox.AgentExecStatus, error) { return statusOut("status: disabled\n") },
			wantCalls: 1,
		},
		{
			// Unparseable output must not stall the provision for the
			// full timeout.
			name:      "unrecognized output proceeds",
			replyN:    func(int) (*proxmox.AgentExecStatus, error) { return statusOut("cloud-init: command not found\n") },
			wantCalls: 1,
		},
		{
			// The exact condition being waited out: the agent restarts
			// mid-probe. Transient, so keep polling.
			name: "survives the agent restart it exists to wait out",
			replyN: func(n int) (*proxmox.AgentExecStatus, error) {
				switch n {
				case 1:
					return statusOut("status: running\n")
				case 2:
					return nil, proxmox.ErrAgentPIDGone
				case 3:
					return nil, errors.New("QEMU guest agent is not running")
				default:
					return statusOut("status: done\n")
				}
			},
			wantCalls: 4,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &stubRunner{replyN: c.replyN}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := provision.WaitForCloudInit(ctx, r, "node1", 106, time.Millisecond); err != nil {
				t.Fatalf("WaitForCloudInit: %v", err)
			}
			if got := int(r.calls.Load()); got != c.wantCalls {
				t.Errorf("probe count = %d, want %d", got, c.wantCalls)
			}
		})
	}
}

// A cloud-init that never finishes must surface as a deadline, naming
// the state it was stuck in.
func TestWaitForCloudInit_DeadlineNamesLastState(t *testing.T) {
	t.Parallel()
	r := &stubRunner{replyN: func(int) (*proxmox.AgentExecStatus, error) { return statusOut("status: running\n") }}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	err := provision.WaitForCloudInit(ctx, r, "node1", 106, time.Millisecond)
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("error should name the stuck state, got: %v", err)
	}
}

// The whole point of the gate: nothing touches the guest before
// cloud-init has been checked. Asserts ordering, not just presence.
func TestProvision_CloudInitProbeRunsBeforeAnyGuestCommand(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)

	var mu sync.Mutex
	var commands []string
	fake.agentRun = func(_ context.Context, _ string, _ int, cmd []string, _ string, _ time.Duration) (*proxmox.AgentExecStatus, error) {
		mu.Lock()
		commands = append(commands, strings.Join(cmd, " "))
		mu.Unlock()
		return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0, OutData: "status: done\n"}, nil
	}
	svc, _, _ := newTestService(t, fake)

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "gated-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) == 0 {
		t.Fatal("no agent commands ran at all")
	}
	if !strings.Contains(commands[0], "cloud-init") {
		t.Errorf("first guest command was %q; the cloud-init gate must run before anything else touches the guest (all commands: %v)",
			commands[0], commands)
	}
}
