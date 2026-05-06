package provision

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// pveCloudInitTabWarning is prepended to a Nimbus VM's description so
// admins inspecting the VM in PVE's web UI see — above the tabs —
// that the Cloud-Init tab there is decorative. The real cloud-init
// payload is delivered via the CD-ROM at ide2 (per-VM ISO Nimbus
// generates and uploads). Edits to the tab silently won't apply.
const pveCloudInitTabWarning = `⚠ Managed by Nimbus — the "Cloud-Init" tab in this UI is decorative. ` +
	`The real cloud-init is delivered via the per-VM ISO at ide2; ` +
	`edits to the tab fields will not apply on next boot. Change settings in Nimbus.`

// ciInstallResult captures what installCIDataISO did, for the caller
// to thread back into Result. All fields are best-effort: the VM
// still provisions when ISO upload fails (the Cloud-Init tab fields
// from SetCloudInit fall back to Proxmox's auto cloud-init drive).
type ciInstallResult struct {
	Attached        bool   // ISO was uploaded and attached to ide2
	ConsolePassword string // surfaced on result page when Attached
	Error           string // human-readable failure reason; surfaced on result page
}

// installCIDataISO builds, uploads, and attaches the per-VM
// cloud-init ISO. On failure logs and continues — the VM still
// provisions, but without our user-data (qga won't auto-install,
// readiness check will time out). The Error field carries the
// failure reason for the result-page warning.
func (s *Service) installCIDataISO(ctx context.Context, node string, vmid int, in CIDataInput) ciInstallResult {
	if s.cfg.CIDataStorage == "" {
		return ciInstallResult{Error: "CIDataStorage unset (NIMBUS_CIDATA_STORAGE) — qga won't auto-install"}
	}

	body, err := BuildCIDataISO(in)
	if err != nil {
		log.Printf("provision vmid=%d: build cidata iso: %v (cidata skipped)", vmid, err)
		return ciInstallResult{Error: "build cidata iso: " + err.Error()}
	}

	filename := vmISOFilename(vmid)
	if err := s.px.UploadFile(ctx, node, s.cfg.CIDataStorage, "iso", filename, body); err != nil {
		log.Printf("provision vmid=%d: upload cidata %s on %s: %v (cidata skipped — qga won't auto-install)",
			vmid, filename, node, err)
		return ciInstallResult{Error: "upload cidata: " + err.Error()}
	}

	// Detach ide2 first. Templates created with the older bootstrap
	// path have a `<storage>:cloudinit` drive at ide2; clones inherit
	// it. Proxmox silently refuses to *replace* a cloudinit drive
	// via a simple ide2= POST — the slot has to be deleted first or
	// the cloudinit volume stays put and our AttachCDROM appears to
	// have done nothing. Best-effort: if ide2 is already empty (new
	// templates), this is a no-op success.
	if err := s.px.DetachDrive(ctx, node, vmid, "ide2"); err != nil {
		// Don't bail — could be ide2 was empty, which detach treats
		// as an error on some PVE versions. The subsequent attach
		// will surface the real problem if there is one.
		log.Printf("provision vmid=%d: detach ide2 (best-effort): %v", vmid, err)
	}

	volid := fmt.Sprintf("%s:iso/%s", s.cfg.CIDataStorage, filename)
	if err := s.px.AttachCDROM(ctx, node, vmid, "ide2", volid); err != nil {
		log.Printf("provision vmid=%d: attach cidata cdrom: %v", vmid, err)
		return ciInstallResult{Error: "attach cidata cdrom: " + err.Error()}
	}
	log.Printf("provision vmid=%d: cidata attached at ide2=%s", vmid, volid)
	return ciInstallResult{
		Attached:        true,
		ConsolePassword: in.ConsolePassword,
	}
}

// deleteCIDataISO removes the per-VM cloud-init ISO at VM
// destruction. Best-effort — leftover ISOs are harmless (~few KB
// each), but cleanup keeps the storage list tidy.
func (s *Service) deleteCIDataISO(ctx context.Context, node string, vmid int) {
	if s.cfg.CIDataStorage == "" {
		return
	}
	volid := fmt.Sprintf("%s:iso/%s", s.cfg.CIDataStorage, vmISOFilename(vmid))
	if err := s.px.DeleteStorageVolume(ctx, node, volid); err != nil {
		log.Printf("cleanup cidata vmid=%d: %v (continuing — file is harmless)", vmid, err)
	}
}

// splitNameservers parses the Nameserver config (space-separated
// list per Proxmox's convention, e.g. "1.1.1.1 8.8.8.8") into a
// slice for cloud-init's network-config — which expects a list.
func splitNameservers(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Fields(s)
}
