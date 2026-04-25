package db

import (
	"gorm.io/gorm"
)

// VM is the canonical record for a provisioned virtual machine.
//
// VMID is the Proxmox cluster-wide identifier. Hostname and IP are unique to
// avoid collisions visible to the user. OwnerID is nullable in Phase 1 (no
// auth) and gains meaning when OAuth lands.
type VM struct {
	gorm.Model
	VMID       int    `gorm:"column:vmid;uniqueIndex;not null"        json:"vmid"`
	Hostname   string `gorm:"column:hostname;uniqueIndex;not null"    json:"hostname"`
	IP         string `gorm:"column:ip;index;not null"                json:"ip"`
	Node       string `gorm:"column:node;not null"                    json:"node"`
	Tier       string `gorm:"column:tier;not null"                    json:"tier"`
	OSTemplate string `gorm:"column:os_template;not null"             json:"os_template"`
	Username   string `gorm:"column:username"                         json:"username"`
	Status     string `gorm:"column:status;index;not null"            json:"status"`
	OwnerID    *uint  `gorm:"column:owner_id;index"                   json:"owner_id,omitempty"`
	ErrorMsg   string `gorm:"column:error_msg"                        json:"error_msg,omitempty"`
}
