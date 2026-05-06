package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AgentExecStatus is the parsed shape of GET /agent/exec-status — the
// terminal state of a previously submitted exec.
//
// Exited == 0 means the command is still running. The boolean-ish
// `out-truncated`/`err-truncated` flags indicate whether the agent
// dropped data at its 4 MB-per-stream cap; callers that care should
// surface this rather than silently keeping the head.
type AgentExecStatus struct {
	Exited       int    `json:"exited"`
	ExitCode     int    `json:"exitcode"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated int    `json:"out-truncated"`
	ErrTruncated int    `json:"err-truncated"`
}

// agentExecResponse wraps the POST response — Proxmox returns just `{pid}`.
type agentExecResponse struct {
	Pid int `json:"pid"`
}

// AgentExec submits a command for execution by the in-guest
// qemu-guest-agent and returns the PID. command[0] is the binary;
// command[1:] are its arguments. inputData is fed to the command's
// stdin (empty string = no stdin).
//
// Proxmox accepts the command as repeated form-encoded values
// (`command=foo&command=bar`); url.Values.Add does the right thing.
func (c *Client) AgentExec(ctx context.Context, node string, vmid int, command []string, inputData string) (int, error) {
	if len(command) == 0 {
		return 0, errors.New("agent exec: empty command")
	}
	params := url.Values{}
	for _, arg := range command {
		params.Add("command", arg)
	}
	if inputData != "" {
		params.Set("input-data", inputData)
	}
	var res agentExecResponse
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, params, &res); err != nil {
		return 0, err
	}
	return res.Pid, nil
}

// AgentExecStatus reads exec-status for a PID. status.Exited == 0
// means the command is still running; callers should poll.
func (c *Client) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (*AgentExecStatus, error) {
	params := url.Values{}
	params.Set("pid", strconv.Itoa(pid))
	var res AgentExecStatus
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodGet, path, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// AgentRun is the convenience wrapper Nimbus's bootstrap paths use:
// submit + poll until exited (or ctx expires) + return final status.
//
// pollInterval defaults to 2 s. The data path is virtio-serial via the
// host hypervisor — no L3 reach to the VM is required, which is the
// whole reason this helper exists. (SSH-from-Nimbus stops working the
// moment we put the VM behind an isolated SDN subnet; agent/exec
// keeps working.)
func (c *Client) AgentRun(ctx context.Context, node string, vmid int, command []string, inputData string, pollInterval time.Duration) (*AgentExecStatus, error) {
	pid, err := c.AgentExec(ctx, node, vmid, command, inputData)
	if err != nil {
		return nil, err
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	// First check is immediate — short scripts often finish before the
	// first tick fires, and a wasted poll is cheaper than a wasted 2 s.
	for {
		status, err := c.AgentExecStatus(ctx, node, vmid, pid)
		if err != nil {
			return nil, err
		}
		if status.Exited != 0 {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
