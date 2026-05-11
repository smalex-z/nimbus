package provision

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"nimbus/internal/bootstrap"
	internalerrors "nimbus/internal/errors"
)

// vmISOFilename is the per-VM cloud-init ISO name produced by the
// pre-D-boot provision path. Retained for swapLegacyCIToManaged to
// recognize legacy VMs that still have one attached at ide2; new
// provisions never create this file.
func vmISOFilename(vmid int) string {
	return fmt.Sprintf("nimbus-vm-%d.iso", vmid)
}

// deleteLegacyCIDataISO removes a pre-D-boot per-VM cidata ISO at VM
// destruction. Called from the destroy path so any straggler files
// (VMs that were never offline-migrated, so their ISO never got swept)
// don't accumulate in the storage list. Best-effort —
// DeleteStorageVolume swallows 404, so calling this against a VM
// that's already been swapped or never had one is harmless.
func (s *Service) deleteLegacyCIDataISO(ctx context.Context, node string, vmid int) {
	if s.cfg.CIDataStorage == "" {
		return
	}
	volid := fmt.Sprintf("%s:iso/%s", s.cfg.CIDataStorage, vmISOFilename(vmid))
	if err := s.px.DeleteStorageVolume(ctx, node, volid); err != nil {
		log.Printf("cleanup legacy cidata vmid=%d: %v (continuing — file is harmless)", vmid, err)
	}
}

// managedCIResult is the outcome of installManagedCloudInit. ConsolePassword
// carries the one-time noVNC password Nimbus generated and pushed via the
// managed drive's cipassword field; the result-page surfaces it on success.
// Error is a human-readable failure reason for the same surface — non-empty
// means the managed drive setup didn't fully succeed but the VM is otherwise
// healthy.
type managedCIResult struct {
	ConsolePassword string
	Error           string
}

// installManagedCloudInit attaches a Proxmox-managed cloud-init drive to the
// fresh clone at ide2. After this call, SetCloudInit (in the caller) writes
// the actual fields (ciuser, sshkeys, ipconfig0, …) which Proxmox bakes into
// the managed drive's contents on every regenerate.
//
// The drive is backed by the same storage as the boot disk so it travels
// with the VM across migrations — Proxmox handles managed-drive migration
// natively, regenerating the drive on the target node from the VM config.
//
// Best-effort failures: any error logs and returns a populated Error so the
// caller can surface it to the operator. The VM is otherwise complete; the
// operator can attach the drive manually via PVE's UI or by re-running this
// path.
//
// Why detach-first: clones that inherit a cloudinit drive from older
// templates (or from anywhere ide2 might already be occupied) would
// silently keep the old drive — PVE's config endpoint refuses to replace a
// `<storage>:cloudinit` reference via a simple ide2= POST. The detach is
// best-effort because empty slots return success on most PVE versions but
// some error; the subsequent attach surfaces any real problem.
func (s *Service) installManagedCloudInit(ctx context.Context, node string, vmid int, consolePassword string) managedCIResult {
	bootStorage, err := s.discoverBootDiskStorage(ctx, node, vmid)
	if err != nil {
		log.Printf("provision vmid=%d: discover boot disk storage: %v", vmid, err)
		return managedCIResult{Error: "discover boot storage: " + err.Error()}
	}

	if err := s.px.DetachDrive(ctx, node, vmid, "ide2"); err != nil {
		// Empty ide2 on a fresh clone is the common case; some PVE
		// versions surface that as an error. The attach below is what
		// actually matters — if the slot was truly occupied with
		// something we can't replace, attach will fail loudly.
		log.Printf("provision vmid=%d: detach ide2 (best-effort): %v", vmid, err)
	}

	if err := s.px.AttachCloudInitDrive(ctx, node, vmid, "ide2", bootStorage); err != nil {
		log.Printf("provision vmid=%d: attach managed cloudinit on %s: %v", vmid, bootStorage, err)
		return managedCIResult{Error: "attach managed cloudinit: " + err.Error()}
	}
	log.Printf("provision vmid=%d: managed cloudinit attached at ide2=%s:cloudinit", vmid, bootStorage)
	return managedCIResult{ConsolePassword: consolePassword}
}

// verifyTemplateBaked checks that the cloned-from template was built with
// the D-boot bake ceremony (qemu-guest-agent pre-installed, cloud-init
// state wiped, marker tag stamped). Pre-D-boot templates can't be cloned
// usefully under the new provision flow because the managed cloud-init
// drive can't install packages — without QGA already in the image,
// WaitForIP would never see an agent come up.
//
// Returns a ValidationError pointing the operator at the bootstrap rebuild
// step. The error message is the source of truth for what an operator sees
// in the API response; keep it actionable.
func (s *Service) verifyTemplateBaked(ctx context.Context, node string, templateVMID int) error {
	cfg, err := s.px.GetVMConfig(ctx, node, templateVMID)
	if err != nil {
		return fmt.Errorf("read template %d config on %s: %w", templateVMID, node, err)
	}
	tags, _ := cfg["tags"].(string)
	// PVE stores tags as a ';'-separated string. The exact separator is
	// observed across PVE 7.x and 8.x; if a future PVE switches to ','
	// we'd need to widen the split set.
	for _, t := range strings.Split(tags, ";") {
		if strings.TrimSpace(t) == bootstrap.NimbusBakedTag {
			return nil
		}
	}
	return &internalerrors.ValidationError{
		Field: "template",
		Message: fmt.Sprintf(
			"template VMID %d on %s was built before this Nimbus version (missing %q tag). "+
				"Re-run bootstrap (Settings → Storage → rebuild templates) to bake qemu-guest-agent into the cloud image. "+
				"Existing VMs are unaffected — they keep working until the operator migrates or renumbers them, "+
				"at which point Nimbus will auto-swap them onto the managed cloud-init drive.",
			templateVMID, node, bootstrap.NimbusBakedTag),
	}
}

// swapLegacyCIToManaged converts a legacy (pre-D-boot) VM's cloud-init
// delivery from the per-node Nimbus cidata ISO to a Proxmox-managed
// cloudinit drive. The two reasons we need this:
//
//  1. Proxmox refuses migration of VMs with local CDROM ISOs attached,
//     so any legacy VM is permanently pinned to its source node until
//     we detach the ISO.
//
//  2. Renumber/force-gateway needs SetCloudInit(ipconfig0) to actually
//     reach the guest. With only the Nimbus cidata ISO as datasource,
//     the field is decorative; with a Proxmox managed drive in place,
//     it's load-bearing.
//
// Idempotent. Three relevant states for ide2:
//   - already a managed cloudinit drive ("<storage>:cloudinit") → no-op
//   - empty                                                      → attach managed drive
//   - matches our legacy nimbus-vm-{vmid}.iso pattern             → detach + delete + attach managed drive
//   - anything else (user-attached ISO / unknown CDROM)           → refuse with a clear error
//
// Fields like ciuser / sshkeys / ipconfig0 are NOT repopulated here.
// Legacy VMs had these set during their original provision via the
// SetCloudInit call (then decorative because there was no managed
// drive consuming them). The values are still in the VM config and
// drive the managed drive's contents on its first regenerate.
//
// Best-effort on the ISO file delete: a leftover .iso costs a few KB
// of storage and DeleteStorageVolume is idempotent on 404.
func (s *Service) swapLegacyCIToManaged(ctx context.Context, node string, vmid int) error {
	cfg, err := s.px.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("get vm config: %w", err)
	}
	ide2, _ := cfg["ide2"].(string)

	// Already managed — nothing to do (new VMs from post-D-boot
	// provision land here straight away).
	if strings.Contains(ide2, ":cloudinit") {
		return nil
	}

	// ide2 has something other than a managed drive. Decide whether
	// it's our legacy ISO (safe to remove) or a user-attached drive
	// (refuse — operator must clear it themselves).
	if ide2 != "" {
		expectedVolid := fmt.Sprintf("%s:iso/%s", s.cfg.CIDataStorage, vmISOFilename(vmid))
		if s.cfg.CIDataStorage == "" || !strings.HasPrefix(ide2, expectedVolid) {
			return fmt.Errorf("ide2 holds %q which is not the Nimbus cidata ISO pattern (%q); refusing to detach — operator must clear ide2 manually before this VM can be migrated or renumbered",
				ide2, expectedVolid)
		}
		if err := s.px.DetachDrive(ctx, node, vmid, "ide2"); err != nil {
			return fmt.Errorf("detach legacy cidata at ide2: %w", err)
		}
		// File cleanup is best-effort — DeleteStorageVolume swallows
		// 404, so this is safe to call even if the file isn't there.
		if delErr := s.px.DeleteStorageVolume(ctx, node, expectedVolid); delErr != nil {
			log.Printf("swap legacy cidata vmid=%d: delete iso file %s: %v (continuing — file is harmless)", vmid, expectedVolid, delErr)
		}
		log.Printf("swap legacy cidata vmid=%d: detached + deleted %s", vmid, expectedVolid)
	}

	// Attach the managed drive on the boot disk's storage so it
	// travels with the VM (Proxmox handles managed-drive migration
	// natively — no local-CDROM blockage like the legacy ISO had).
	bootStorage, err := s.discoverBootDiskStorage(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("discover boot storage: %w", err)
	}
	if err := s.px.AttachCloudInitDrive(ctx, node, vmid, "ide2", bootStorage); err != nil {
		return fmt.Errorf("attach managed cloudinit on %s: %w", bootStorage, err)
	}
	log.Printf("swap legacy cidata vmid=%d: managed cloudinit drive attached at ide2=%s:cloudinit", vmid, bootStorage)
	return nil
}

// discoverBootDiskStorage finds the Proxmox storage backing the VM's boot
// disk by parsing its config. Used to pick where the managed cloud-init
// drive lives — co-locating on the boot disk's storage keeps the drive
// portable across migration without operator config.
//
// Walk slot priority: scsi0 (the convention bootstrap creates), then
// virtio0 / sata0 / ide0 as fallbacks for hand-rolled templates. Returns
// an error when nothing recognizable is found — that means the template
// has no boot disk, which is a setup problem rather than a code problem.
func (s *Service) discoverBootDiskStorage(ctx context.Context, node string, vmid int) (string, error) {
	cfg, err := s.px.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return "", fmt.Errorf("get vm config: %w", err)
	}
	for _, slot := range []string{"scsi0", "virtio0", "sata0", "ide0"} {
		raw, _ := cfg[slot].(string)
		if raw == "" {
			continue
		}
		// Config value shape is "<storage>:<volname>[,size=...,...]". The
		// storage portion ends at the first colon.
		if idx := strings.IndexByte(raw, ':'); idx > 0 {
			return raw[:idx], nil
		}
	}
	return "", errors.New("no recognizable boot disk slot (scsi0/virtio0/sata0/ide0) in vm config")
}
