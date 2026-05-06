package ippool

import "time"

// Status values for an IP allocation.
const (
	StatusFree      = "free"
	StatusReserved  = "reserved"
	StatusAllocated = "allocated"
)

// Source values describe where the row's allocation state came from.
const (
	SourceLocal    = "local"    // claimed by this Nimbus instance via Reserve/MarkAllocated
	SourceAdopted  = "adopted"  // observed in Proxmox by the reconciler and adopted into the local cache
	SourceExternal = "external" // detected on the LAN by the netscan loop — not Nimbus-managed and not in Proxmox
)

// IPAllocation is a single row in the IP pool table. The IP itself is the
// natural primary key — there is no surrogate ID and no soft delete: a freed
// IP is set back to "free" rather than removed, because we want to know which
// addresses are part of the pool independently of allocation state.
//
// VNet scopes the row to a specific Proxmox SDN VNet. Empty string ("") is
// the legacy global vmbr0 pool — preserved for non-SDN deployments and for
// VMs provisioned before SDN landed. Non-empty values name the user's
// per-subnet pool. Two pools cannot share an IP by construction: the SDN
// supernet-carving algorithm hands out non-overlapping /N CIDRs per user,
// and the legacy global pool's range is configured separately. So IP can
// stay as the PK without a composite-key migration; VNet is purely
// routing context for the reconciler + allocator.
type IPAllocation struct {
	IP           string     `gorm:"column:ip;primaryKey;size:45"     json:"ip"`
	VNet         string     `gorm:"column:vnet;not null;default:'';index" json:"vnet,omitempty"`
	Status       string     `gorm:"column:status;not null;index"     json:"status"`
	VMID         *int       `gorm:"column:vmid;index"                json:"vmid,omitempty"`
	Hostname     *string    `gorm:"column:hostname"                  json:"hostname,omitempty"`
	ReservedAt   *time.Time `gorm:"column:reserved_at"               json:"reserved_at,omitempty"`
	AllocatedAt  *time.Time `gorm:"column:allocated_at"              json:"allocated_at,omitempty"`
	LastSeenAt   *time.Time `gorm:"column:last_seen_at"              json:"last_seen_at,omitempty"`
	Source       string     `gorm:"column:source;default:local"      json:"source,omitempty"`
	MissedCycles int        `gorm:"column:missed_cycles;default:0"   json:"missed_cycles"`
}

// TableName overrides GORM's pluralization to keep the schema name explicit.
func (IPAllocation) TableName() string { return "ip_allocations" }
