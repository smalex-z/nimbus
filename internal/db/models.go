package db

import (
	"time"

	"gorm.io/gorm"
)

// User represents an account in Nimbus.
type User struct {
	gorm.Model
	Name         string `gorm:"not null" json:"name"`
	Email        string `gorm:"uniqueIndex;not null" json:"email"`
	PasswordHash string `gorm:"default:''" json:"-"`
	IsAdmin      bool   `gorm:"default:false" json:"is_admin"`
	// VerifiedCodeVersion is the AccessCodeVersion the user last verified
	// against. When the admin regenerates the access code, the version
	// increments and this user's verification becomes stale, forcing them
	// back through the verify form on their next action.
	VerifiedCodeVersion int `gorm:"default:0" json:"-"`
}

// Session ties a browser cookie to a user for a limited duration.
type Session struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"not null;index" json:"user_id"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	CreatedAt time.Time
}

// OAuthSettings stores OAuth provider credentials. Only a single row (ID=1) is used.
// AccessCode is the 8-digit second-factor code non-admin users must enter to
// gain access to provisioning features. AccessCodeVersion increments each time
// the admin regenerates it, invalidating prior user verifications.
type OAuthSettings struct {
	ID                 uint   `gorm:"primaryKey"`
	GitHubClientID     string `gorm:"default:''"`
	GitHubClientSecret string `gorm:"default:''"`
	GoogleClientID     string `gorm:"default:''"`
	GoogleClientSecret string `gorm:"default:''"`
	AccessCode         string `gorm:"default:''"`
	AccessCodeVersion  int    `gorm:"default:0"`
	// AuthorizedGoogleDomains is a comma-separated list of lowercased email
	// domains. Google OAuth sign-ins from these domains bypass the access
	// code requirement and are auto-verified on every login. When empty, no
	// bypass is granted and Google sign-ups behave like email/password
	// (subject to the access code).
	AuthorizedGoogleDomains string `gorm:"default:''"`
}

// VM is the canonical record for a provisioned virtual machine.
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
	// SSHKeyID points to the row in the ssh_keys table that owns the SSH
	// material for this VM. nullable: set NULL when the key is deleted, so the
	// VM record outlives its key. Replaces the legacy per-VM KeyName/CT/Nonce
	// columns going forward.
	SSHKeyID *uint `gorm:"column:ssh_key_id;index"                       json:"ssh_key_id,omitempty"`
	// KeyName is denormalized for convenient list rendering — clients still
	// want "ssh -i ~/.ssh/{key_name}" without joining to ssh_keys. Set to the
	// linked key's Name at provision time.
	KeyName string `gorm:"column:key_name"                                 json:"key_name,omitempty"`
	// Public half of the SSH key, stored as the raw "ssh-<algo> <base64> <comment>" line.
	SSHPubKey string `gorm:"column:ssh_pubkey"                       json:"ssh_pubkey,omitempty"`
	// Legacy encrypted private-key columns. New rows do NOT populate these —
	// they live in the ssh_keys table now. Kept on the model so the startup
	// migration can read them, then NULL them out. Will be dropped in a
	// follow-up once the migration has run on all environments.
	SSHPrivKeyCT    []byte `gorm:"column:ssh_privkey_ct"                json:"-"`
	SSHPrivKeyNonce []byte `gorm:"column:ssh_privkey_nonce"             json:"-"`
	ErrorMsg        string `gorm:"column:error_msg"                     json:"error_msg,omitempty"`
}

// SSHKey is a first-class user-managed SSH key.
//
// Public key is always stored. The private half is optional: when populated,
// it's AES-256-GCM ciphertext with the nonce alongside (encryption key lives
// in env config, not the DB). Source records the key's provenance:
//   - "imported": the user pasted/uploaded a public key (and optionally the private half).
//   - "generated": Nimbus minted the keypair on the user's behalf.
//   - "vm-auto": legacy per-VM vault entry migrated into this table on startup.
//
// IsDefault is mutually exclusive within an OwnerID — the service is
// responsible for clearing the previous default when a new one is set.
type SSHKey struct {
	gorm.Model
	Name        string `gorm:"column:name;uniqueIndex;not null"        json:"name"`
	Label       string `gorm:"column:label"                            json:"label,omitempty"`
	PublicKey   string `gorm:"column:public_key;not null"              json:"public_key"`
	Fingerprint string `gorm:"column:fingerprint;index"                json:"fingerprint,omitempty"`
	// Encrypted private half. Zero-length when the key was imported public-only.
	PrivKeyCT    []byte `gorm:"column:priv_key_ct"                     json:"-"`
	PrivKeyNonce []byte `gorm:"column:priv_key_nonce"                  json:"-"`
	IsDefault    bool   `gorm:"column:is_default;index"                json:"is_default"`
	OwnerID      *uint  `gorm:"column:owner_id;index"                  json:"owner_id,omitempty"`
	Source       string `gorm:"column:source"                          json:"source,omitempty"`
}

// HasPrivateKey reports whether this key has a vaulted private half. Used by
// list/detail responses to drive the "Download" affordance.
func (k *SSHKey) HasPrivateKey() bool {
	return len(k.PrivKeyCT) > 0 && len(k.PrivKeyNonce) > 0
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
