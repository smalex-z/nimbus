package ippool

import "time"

// Status values for an IP allocation.
const (
	StatusFree      = "free"
	StatusReserved  = "reserved"
	StatusAllocated = "allocated"
)

// IPAllocation is a single row in the IP pool table. The IP itself is the
// natural primary key — there is no surrogate ID and no soft delete: a freed
// IP is set back to "free" rather than removed, because we want to know which
// addresses are part of the pool independently of allocation state.
type IPAllocation struct {
	IP          string     `gorm:"column:ip;primaryKey;size:45"   json:"ip"`
	Status      string     `gorm:"column:status;not null;index"   json:"status"`
	VMID        *int       `gorm:"column:vmid;index"              json:"vmid,omitempty"`
	Hostname    *string    `gorm:"column:hostname"                json:"hostname,omitempty"`
	ReservedAt  *time.Time `gorm:"column:reserved_at"             json:"reserved_at,omitempty"`
	AllocatedAt *time.Time `gorm:"column:allocated_at"            json:"allocated_at,omitempty"`
}

// TableName overrides GORM's pluralization to keep the schema name explicit.
func (IPAllocation) TableName() string { return "ip_allocations" }
