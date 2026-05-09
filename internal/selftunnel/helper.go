package selftunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// File-IPC paths shared between the main nimbus service (running as
// the nimbus user, hardened) and the gopher-bootstrap helper unit
// (running as root, unhardened, fired by a systemd path unit). The
// directory is part of the main service's ReadWritePaths so the
// nimbus user can drop pending.json + the trigger; root can read all
// files in /var/lib/nimbus by default.
const (
	pendingPath = "/var/lib/nimbus/bootstrap-pending.json"
	triggerPath = "/var/lib/nimbus/bootstrap-trigger"
	resultPath  = "/var/lib/nimbus/bootstrap-result.json"
)

// helperPending is the payload nimbus.service writes for the helper
// to consume. InstanceID is reserved for the multi-Gopher future
// (today the only legal value is "default"); the helper passes it
// back unchanged on the result so a future caller can correlate.
type helperPending struct {
	InstanceID   string `json:"instance_id"`
	BootstrapURL string `json:"bootstrap_url"`
	MachineName  string `json:"machine_name"`
}

// helperResult is what the helper writes back. Output captures the
// bootstrap script's stdout+stderr (truncated to 64 KiB so a runaway
// dpkg trace doesn't blow up the JSON). Error is empty on success.
type helperResult struct {
	InstanceID string `json:"instance_id"`
	Success    bool   `json:"success"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
	FinishedAt string `json:"finished_at"`
}

// RunHelper is the entry point for `nimbus gopher-bootstrap`. The
// systemd path unit fires when /var/lib/nimbus/bootstrap-trigger is
// touched; this function reads the pending payload, runs the install,
// and writes the result back. Always writes a result file (even on
// failure) so the polling main service doesn't hang.
//
// Detached from the rest of selftunnel.Service — runs in a separate
// process under root, so it has no access to settings/db/etc. The
// only contract is "what's in pending.json comes out of result.json
// after a curl|bash."
func RunHelper() error {
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		return fmt.Errorf("read pending: %w", err)
	}
	var pending helperPending
	if err := json.Unmarshal(raw, &pending); err != nil {
		return fmt.Errorf("parse pending: %w", err)
	}
	if pending.BootstrapURL == "" {
		_ = writeResult(helperResult{
			InstanceID: pending.InstanceID,
			Success:    false,
			Error:      "pending payload missing bootstrap_url",
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return fmt.Errorf("missing bootstrap_url")
	}

	// Run the install. The helper service unit doesn't set
	// NoNewPrivileges or ProtectSystem, so apt + systemd drops work
	// here. GOPHER_MACHINE_NAME skips the bootstrap script's
	// interactive hostname prompt.
	cmd := exec.Command("bash", "-c", fmt.Sprintf(
		"curl -fsSL %s | GOPHER_MACHINE_NAME=%s bash",
		shellQuoteHelper(pending.BootstrapURL),
		shellQuoteHelper(pending.MachineName),
	))
	out, runErr := cmd.CombinedOutput()

	res := helperResult{
		InstanceID: pending.InstanceID,
		Success:    runErr == nil,
		Output:     truncateHelper(string(out), 64*1024),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if runErr != nil {
		res.Error = runErr.Error()
	}
	if err := writeResult(res); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if runErr != nil {
		return fmt.Errorf("bootstrap script: %w", runErr)
	}
	return nil
}

// writeResult persists the helper's outcome. Mode 0644 so the nimbus
// user (which polls for the result) can read; root wrote it.
func writeResult(r helperResult) error {
	buf, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// Atomic rename so the nimbus user's poll never sees a half-
	// written JSON file.
	tmp := resultPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, resultPath)
}

// shellQuoteHelper is identical to the package-private shellQuote in
// service.go; duplicated so the helper compiles without dragging the
// rest of the service into the binary's helper-only path.
func shellQuoteHelper(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func truncateHelper(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…[%d bytes truncated]", len(s)-n)
}
