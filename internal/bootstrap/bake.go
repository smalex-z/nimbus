package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kdomanski/iso9660"
)

// NimbusBakedTag is the VM tag bootstrap stamps on a template after a
// successful bake. The provision flow reads this to verify the template
// was built with qemu-guest-agent pre-installed (i.e. the post-D-boot
// template generation). VMs cloned from an unstamped template fail with
// a clear "re-run bootstrap to rebuild" error rather than the older
// silent breakage.
//
// The version suffix gives us room to invalidate older bake formats in
// the future without confusing the marker semantics.
const NimbusBakedTag = "nimbus-baked-v1"

// Bake timing knobs. These are tunable here rather than in Config because
// they're calibrated against the bake workload, not deployment-specific.
const (
	bakeStartTaskPoll = 2 * time.Second // start-task UPID poll
	bakeQGAReadyPoll  = 3 * time.Second // delay between QGA-availability probes
	bakeQGAReadyMax   = 5 * time.Minute // QGA must come up within this — covers apt install qemu-guest-agent over a slow mirror
	bakeCloudInitMax  = 5 * time.Minute // cloud-init status --wait deadline AFTER QGA is up
	bakeAgentExecPoll = 2 * time.Second // AgentRun internal poll
	bakeShutdownPoll  = 3 * time.Second // status poll while we wait for shutdown to complete
	bakeShutdownMax   = 3 * time.Minute // QGA-issued poweroff is quick; bound the wait
)

// bakeTemplate runs the one-time "configure-then-template" ceremony on a
// freshly-imported VM disk before ConvertToTemplate. After bakeTemplate
// returns nil the VM is:
//
//   - powered off,
//   - has qemu-guest-agent installed and enabled (will start on next boot),
//   - has cloud-init state wiped (clean instance-id, no cached datasource),
//   - has no transient bake ISO attached.
//
// Operators don't see this ceremony — it runs end-to-end via the Proxmox
// API. If it fails, the bake VM is force-stopped so it doesn't eat
// cluster resources; the caller surfaces the error and the operator can
// destroy / re-run bootstrap.
//
// Ordering note: we drive the cleanup (cloud-init clean) and shutdown
// from outside cloud-init via QGA exec, not from inside cloud-init's
// own user-data. Doing it from inside cloud-init races against late-
// stage modules (final-message, power-state-change) that may try to
// write back into /var/lib/cloud/instances/* after we've already
// `cloud-init clean`-ed it.
func (s *Service) bakeTemplate(ctx context.Context, node string, vmid int) error {
	// Build the bake ISO. Tiny (~few KB), all in memory.
	isoBytes, err := buildBakeISO(vmid)
	if err != nil {
		return fmt.Errorf("build bake iso: %w", err)
	}
	isoFilename := fmt.Sprintf("nimbus-template-bake-%d.iso", vmid)
	volid := fmt.Sprintf("%s:iso/%s", s.cfg.ImageStorage, isoFilename)

	// Best-effort ISO cleanup regardless of success path — file is small
	// but leaks accumulate. Uses a fresh ctx so a parent cancellation
	// doesn't also kill the cleanup.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if delErr := s.px.DeleteStorageVolume(cleanupCtx, node, volid); delErr != nil {
			log.Printf("bake vmid=%d: cleanup bake iso file: %v (continuing)", vmid, delErr)
		}
	}()

	if err := s.px.UploadFile(ctx, node, s.cfg.ImageStorage, "iso", isoFilename, isoBytes); err != nil {
		return fmt.Errorf("upload bake iso: %w", err)
	}
	if err := s.px.AttachCDROM(ctx, node, vmid, "ide2", volid); err != nil {
		return fmt.Errorf("attach bake iso: %w", err)
	}

	// Start the VM. From here on, ANY non-success exit needs to stop the
	// VM so it doesn't burn cluster CPU/RAM while the operator
	// investigates.
	startTask, err := s.px.StartVM(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("bake start vm: %w", err)
	}
	if startTask != "" {
		if err := s.px.WaitForTask(ctx, node, startTask, bakeStartTaskPoll); err != nil {
			return fmt.Errorf("bake start task: %w", err)
		}
	}

	success := false
	defer func() {
		if success {
			return
		}
		// Force-stop a wedged bake VM. We use a fresh ctx so this fires
		// even when the caller's ctx is already canceled.
		stopCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		stopTask, stopErr := s.px.StopVM(stopCtx, node, vmid)
		if stopErr != nil {
			log.Printf("bake vmid=%d: cleanup stop: %v (vm may be left running)", vmid, stopErr)
			return
		}
		if stopTask != "" {
			if waitErr := s.px.WaitForTask(stopCtx, node, stopTask, bakeShutdownPoll); waitErr != nil {
				log.Printf("bake vmid=%d: cleanup stop wait: %v", vmid, waitErr)
			}
		}
	}()

	// Wait for qemu-guest-agent to come up. Cloud-init installs it via
	// the bake ISO's user-data; budget covers apt-update + install on
	// a slow Ubuntu mirror.
	if err := s.waitForQGA(ctx, node, vmid, bakeQGAReadyMax); err != nil {
		return fmt.Errorf("bake qga readiness: %w", err)
	}

	// Block until cloud-init has fully exited (final-stages, runcmd, all
	// done). Without this we could clean+shutdown while cloud-init's
	// post-runcmd modules are still writing state.
	ciCtx, cancel := context.WithTimeout(ctx, bakeCloudInitMax)
	defer cancel()
	ciStatus, err := s.px.AgentRun(ciCtx, node, vmid,
		[]string{"cloud-init", "status", "--wait"}, "", bakeAgentExecPoll)
	if err != nil {
		return fmt.Errorf("bake cloud-init wait: %w", err)
	}
	if ciStatus.ExitCode != 0 {
		return fmt.Errorf("bake cloud-init reported errors: exit=%d stderr=%q",
			ciStatus.ExitCode, strings.TrimSpace(ciStatus.ErrData))
	}

	// Wipe cloud-init state so the cloned VM starts fresh — new
	// instance-id, no cached datasource, no per-instance markers. Must
	// happen before we power off (so the wipe sticks on the next boot
	// of any clone).
	cleanStatus, err := s.px.AgentRun(ctx, node, vmid,
		[]string{"cloud-init", "clean", "--logs", "--seed"}, "", bakeAgentExecPoll)
	if err != nil {
		return fmt.Errorf("bake cloud-init clean: %w", err)
	}
	if cleanStatus.ExitCode != 0 {
		return fmt.Errorf("bake cloud-init clean exit=%d stderr=%q",
			cleanStatus.ExitCode, strings.TrimSpace(cleanStatus.ErrData))
	}

	// Power off via QGA so cleanup happens after cloud-init is fully
	// gone. The agent connection drops mid-poweroff, which AgentRun
	// may surface as an error or a zero exit — we don't care either
	// way, just wait for status=stopped.
	if _, err := s.px.AgentRun(ctx, node, vmid,
		[]string{"poweroff"}, "", bakeAgentExecPoll); err != nil {
		log.Printf("bake vmid=%d: qga poweroff returned %v (expected — polling for stopped)", vmid, err)
	}
	if err := s.waitForStopped(ctx, node, vmid, bakeShutdownMax); err != nil {
		return fmt.Errorf("bake await stopped: %w", err)
	}

	// Detach the bake ISO — Proxmox refuses to migrate VMs with local
	// CDROM ISOs attached, so the template MUST be free of ide2 before
	// ConvertToTemplate.
	if err := s.px.DetachDrive(ctx, node, vmid, "ide2"); err != nil {
		return fmt.Errorf("bake detach ide2: %w", err)
	}

	success = true
	return nil
}

// waitForQGA polls until an AgentRun probe succeeds — the in-guest
// qemu-guest-agent is reachable from the host. The probe is a trivial
// `true` command; we only care that the QGA RPC works, not what the
// command does.
//
// Returns ctx error (timeout / cancel) when the deadline passes without
// QGA ever responding. Other transport errors are retried — Proxmox's
// QGA endpoint 500s while the agent is missing.
func (s *Service) waitForQGA(ctx context.Context, node string, vmid int, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		if _, err := s.px.AgentRun(ctx, node, vmid,
			[]string{"true"}, "", bakeAgentExecPoll); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qga did not respond within %s", max)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bakeQGAReadyPoll):
		}
	}
}

// waitForStopped polls /status/current until the VM reports stopped.
// Returns an error if the deadline passes; ctx cancellation surfaces
// as ctx.Err.
func (s *Service) waitForStopped(ctx context.Context, node string, vmid int, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		state, err := s.px.GetVMState(ctx, node, vmid)
		if err == nil && strings.EqualFold(state, "stopped") {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("vm did not stop within %s (last error: %w)", max, err)
			}
			return fmt.Errorf("vm did not stop within %s (last state: %q)", max, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bakeShutdownPoll):
		}
	}
}

// buildBakeISO renders the cidata ISO Nimbus attaches to a fresh
// template-bake VM. Carries the absolute minimum: install + enable
// qemu-guest-agent, DHCP networking, a pinned instance-id. No user
// account, no SSH keys, no static IP — bootstrap drives the rest via
// QGA exec.
func buildBakeISO(vmid int) ([]byte, error) {
	w, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("iso writer: %w", err)
	}
	defer w.Cleanup() //nolint:errcheck

	userData := `#cloud-config
# Auto-generated by Nimbus bootstrap for one-time template bake.
# Drives nothing user-facing — just installs qemu-guest-agent so
# bootstrap can take over via QGA exec for cleanup + shutdown.
package_update: true
packages:
  - qemu-guest-agent
runcmd:
  - [ systemctl, enable, --now, qemu-guest-agent.service ]
`

	metaData := fmt.Sprintf("instance-id: nimbus-template-bake-%d\nlocal-hostname: nimbus-template-bake\n", vmid)

	// network-config v2: any ethernet interface (matches eth0/ens18/enp0s3
	// across cloud image variants), DHCP only.
	networkConfig := `version: 2
ethernets:
  primary:
    match:
      name: "e*"
    dhcp4: true
`

	if err := w.AddFile(strings.NewReader(userData), "user-data"); err != nil {
		return nil, fmt.Errorf("add user-data: %w", err)
	}
	if err := w.AddFile(strings.NewReader(metaData), "meta-data"); err != nil {
		return nil, fmt.Errorf("add meta-data: %w", err)
	}
	if err := w.AddFile(strings.NewReader(networkConfig), "network-config"); err != nil {
		return nil, fmt.Errorf("add network-config: %w", err)
	}

	var buf bytes.Buffer
	// "cidata" is the volume label cloud-init's NoCloud datasource scans
	// for; lowercase to match Proxmox's convention.
	if err := w.WriteTo(&buf, "cidata"); err != nil {
		return nil, fmt.Errorf("write iso: %w", err)
	}
	return buf.Bytes(), nil
}

// Re-export a sentinel for callers that want to assert on bake failures
// without depending on bake-internal error text shapes.
var ErrBakeFailed = errors.New("template bake failed")
