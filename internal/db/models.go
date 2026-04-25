package db

import (
	"time"

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
	KeyName    string `gorm:"column:key_name"                         json:"key_name,omitempty"`
	// Public half of the SSH key, stored as the raw "ssh-<algo> <base64> <comment>" line.
	SSHPubKey string `gorm:"column:ssh_pubkey"                       json:"ssh_pubkey,omitempty"`
	// Encrypted private half. Empty when the user opted out of key-vault storage
	// (BYO without supplying a private key). Encryption is AES-256-GCM with the
	// nonce stored alongside; the master key lives outside the DB (env var).
	SSHPrivKeyCT    []byte `gorm:"column:ssh_privkey_ct"                json:"-"`
	SSHPrivKeyNonce []byte `gorm:"column:ssh_privkey_nonce"             json:"-"`
	ErrorMsg        string `gorm:"column:error_msg"                     json:"error_msg,omitempty"`
}

// NodeTemplate maps an (OS, node) pair to the Proxmox VMID where that node's
// copy of the template lives. Required because Proxmox VMIDs are cluster-wide
// unique — we can't naively use VMID 9000 for "Ubuntu 24.04 on every node",
// so each node's template gets a Proxmox-assigned VMID, and we look it up at
// provision time by (node, os).
type NodeTemplate struct {
	Node      string    `gorm:"column:node;primaryKey;not null"    json:"node"`
	OS        string    `gorm:"column:os;primaryKey;not null"      json:"os"`
	VMID      int       `gorm:"column:vmid;uniqueIndex;not null"   json:"vmid"`
	CreatedAt time.Time `gorm:"column:created_at"                  json:"created_at"`
}

// TableName pins the table name to avoid GORM's default pluralization choosing
// something unexpected for the unusual struct name.
func (NodeTemplate) TableName() string { return "node_templates" }
