package bootstrap

// OSEntry describes one cloud image Nimbus can bootstrap into a Proxmox
// template. The Offset is added to the operator-configured TemplateBaseVMID
// (default 9000) to compute the final VMID — keeping the catalogue stable
// even if the operator chooses a different VMID base.
//
// The Filename field must match the URL's basename and is what Proxmox stores
// the downloaded file under in the storage's iso content directory (typically
// /var/lib/vz/template/iso/<filename>). It also feeds the import-from path
// when we create the VM.
type OSEntry struct {
	OS          string // user-facing key matching proxmox.TemplateOffsets
	Offset      int    // added to TemplateBaseVMID
	Filename    string // file as stored on the node
	URL         string // canonical cloud-image source
	DisplayName string
}

// Catalog is the canonical set of OSes Nimbus knows how to bootstrap. The
// order here matches the Phase 1 design doc §6.2 mapping (Ubuntu 24.04 →
// 9000, Ubuntu 22.04 → 9001, etc.) and the offsets in proxmox.TemplateOffsets.
//
// NOTE on Ubuntu filenames: the canonical Ubuntu cloud images ship with a
// ".img" extension but are actually qcow2-format files. Proxmox's `import`
// content type rejects ".img" extensions ("invalid filename or wrong
// extension"), so we save them locally as ".qcow2". The URL stays canonical;
// only the on-disk filename differs.
var Catalog = []OSEntry{
	{
		OS:          "ubuntu-24.04",
		Offset:      0,
		Filename:    "ubuntu-24.04-server-cloudimg-amd64.qcow2",
		URL:         "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img",
		DisplayName: "Ubuntu 24.04 LTS",
	},
	{
		OS:          "ubuntu-22.04",
		Offset:      1,
		Filename:    "ubuntu-22.04-server-cloudimg-amd64.qcow2",
		URL:         "https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img",
		DisplayName: "Ubuntu 22.04 LTS",
	},
	{
		OS:          "debian-12",
		Offset:      2,
		Filename:    "debian-12-genericcloud-amd64.qcow2",
		URL:         "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
		DisplayName: "Debian 12 (Bookworm)",
	},
	{
		OS:          "debian-11",
		Offset:      3,
		Filename:    "debian-11-genericcloud-amd64.qcow2",
		URL:         "https://cloud.debian.org/images/cloud/bullseye/latest/debian-11-genericcloud-amd64.qcow2",
		DisplayName: "Debian 11 (Bullseye)",
	},
}

// LookupOS finds a catalog entry by user-facing key. Returns nil if not found.
func LookupOS(os string) *OSEntry {
	for i := range Catalog {
		if Catalog[i].OS == os {
			return &Catalog[i]
		}
	}
	return nil
}

// AllOSKeys returns the full default OS list (used when Request.OS is empty).
func AllOSKeys() []string {
	out := make([]string, len(Catalog))
	for i, e := range Catalog {
		out[i] = e.OS
	}
	return out
}
