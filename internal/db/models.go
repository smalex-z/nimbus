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
	// GitHubOrgs is the comma-separated snapshot of GitHub org logins the
	// user belonged to at their last GitHub OAuth login. Used by the
	// authorized-orgs bypass: IsUserVerified intersects this against the
	// admin's current authorized-orgs list dynamically. Empty for users who
	// have never signed in via GitHub. Also doubles as the
	// GitHub-connected indicator — a non-empty string means the user has
	// completed at least one GitHub OAuth login (org list may be "-" if
	// they belong to no orgs).
	GitHubOrgs string `gorm:"default:''" json:"-"`
	// GoogleConnected is a boolean flag set on every successful Google
	// OAuth login. Used as the "Google account linked" marker on the
	// /account page and by the passwordless-mode straggler check.
	// GoogleSub holds the Google account's stable per-user identifier
	// (the JWT `sub` claim) so subsequent sign-ins match by identity
	// rather than email — needed when the user's Google email differs
	// from their Nimbus email.
	GoogleConnected bool   `gorm:"default:false" json:"-"`
	GoogleSub       string `gorm:"column:google_sub;index;default:''" json:"-"`
	// GitHubID is the GitHub account's stable numeric user id (stored
	// as a string for uniformity with GoogleSub). Same role: identity
	// matching across email changes. GitHubOrgs continues to track org
	// membership for the dynamic bypass; GitHubID is purely about
	// "who is this Nimbus user."
	GitHubID string `gorm:"column:github_id;index;default:''" json:"-"`
	// Suspended locks an account out of every sign-in path (password,
	// Google, GitHub) without deleting their data. Used by the
	// passwordless transition: an admin can suspend password-only
	// users in bulk so the OAuth-only toggle stops being blocked,
	// then unsuspend later (or the user recovers via a future
	// email-based flow). Suspended users' sessions are revoked at
	// suspend time.
	Suspended bool `gorm:"default:false;index" json:"-"`
	// VMQuotaOverride and GPUJobQuotaOverride let an admin grant a
	// specific user a quota above (or below) the workspace default
	// stored on QuotaSettings. Both are nullable pointers: nil means
	// "use the workspace default," a value means "use this number"
	// (zero is allowed and means "this user can't provision/submit").
	// Pointers rather than int with a sentinel because GORM's
	// AutoMigrate fills new columns on existing rows with the zero
	// value, and we'd then have no way to distinguish "no override"
	// from "explicit zero override."
	VMQuotaOverride     *int `gorm:"column:vm_quota_override" json:"-"`
	GPUJobQuotaOverride *int `gorm:"column:gpu_job_quota_override" json:"-"`
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
	// AuthorizedGitHubOrgs is a comma-separated list of lowercased GitHub
	// org logins. Members of any of these orgs bypass the access code on
	// GitHub OAuth sign-in. Empty list disables the GitHub bypass.
	AuthorizedGitHubOrgs string `gorm:"default:''"`
	// RequirePasswordlessAuth is the admin's declared intent to remove
	// password sign-in once every user has linked an OAuth provider. The
	// flag itself doesn't immediately disable the password form — that
	// would lock out users who still need to link. Instead it gates a
	// dynamic check: when this is true AND no users still have only
	// password access, the sign-in page hides the password form.
	RequirePasswordlessAuth bool `gorm:"default:false"`
}

// LoginToken is a single-use, short-lived token that signs a user in
// when redeemed. Currently minted by the magic-link recovery flow
// that emails password-only users (so they can come connect
// Google/GitHub on their /account page) and consumed by
// /api/auth/magic/{token}.
//
// Token is the opaque random-hex string and the primary key. Purpose
// tags what flow minted it so the consumer can refuse cross-purpose
// tokens — today only "magic_link" exists, but the column is here so
// future single-use tokens (password reset, invite) don't need
// another schema bump.
//
// Single-use is enforced by an UPDATE ... SET used_at = ? WHERE
// token = ? AND used_at IS NULL inside ConsumeLoginToken: the row
// can only flip from unused to used once, and a concurrent second
// redeem hits zero rows affected.
type LoginToken struct {
	Token     string     `gorm:"primaryKey"`
	UserID    uint       `gorm:"not null;index"`
	Purpose   string     `gorm:"not null;default:''"`
	ExpiresAt time.Time  `gorm:"not null;index"`
	UsedAt    *time.Time `gorm:"index"`
	CreatedAt time.Time
}

// SMTPSettings holds outbound-mail configuration. Singleton (ID=1).
// Used by the planned magic-link recovery flow that emails password-only
// users when an admin moves the workspace to OAuth-only sign-in. The
// admin populates host/port/from/credentials on the /email page; the
// password is encrypted at rest via the same secrets.Cipher used for
// SSH key vault entries (NIMBUS_ENCRYPTION_KEY-derived AES-256-GCM).
//
// Today the saved config is dormant — no code reads it for sending yet.
// The "Email N unlinked users" button on the Sign-in providers card
// stays disabled until both this is configured AND the send pipeline
// lands in a follow-up release. Storing now (vs. later) guarantees the
// schema doesn't need a destructive migration when send arrives.
type SMTPSettings struct {
	ID            uint   `gorm:"primaryKey"`
	Host          string `gorm:"default:''"`
	Port          int    `gorm:"default:587"`
	Username      string `gorm:"default:''"`
	PasswordCT    []byte `gorm:"column:password_ct"`
	PasswordNonce []byte `gorm:"column:password_nonce"`
	// FromAddress is the envelope sender. Required for sending — until
	// it's set we treat SMTP as "not configured" regardless of host.
	FromAddress string `gorm:"default:''"`
	// Encryption is "starttls" / "tls" / "none". starttls is the right
	// default for port 587 (submission); tls for 465 (smtps); none is
	// only for unauthenticated relays inside trusted networks.
	Encryption string `gorm:"default:'starttls'"`
	// Enabled is the admin's manual on/off switch — flipping it off
	// keeps the credentials but stops the (future) send pipeline from
	// using them. Useful for temporary outages or migrations without
	// wiping the form.
	Enabled bool `gorm:"default:false"`
}

// QuotaSettings stores workspace-wide quota defaults. Singleton (ID=1).
// Members hit these caps; admins bypass via Request.RequesterIsAdmin.
// Per-user overrides on db.User.VMQuotaOverride /
// db.User.GPUJobQuotaOverride layer on top — the effective cap is the
// override when present, else the value here.
//
// Seeded on first boot from the legacy hardcoded constants
// (provision.MemberMaxVMs, gpu.MemberMaxActiveJobs) so an upgrade
// doesn't change observed behaviour. Future schema bumps that add new
// quota dimensions (CPU cores, RAM, etc.) extend this row rather than
// introducing parallel tables.
type QuotaSettings struct {
	ID                  uint `gorm:"primaryKey"`
	MemberMaxVMs        int  `gorm:"default:5"`
	MemberMaxActiveJobs int  `gorm:"default:5"`
}

// SchedulingSettings stores cluster-wide overcommit ratios the scheduler
// uses when deciding whether a node can host a tier. Only a single row
// (ID=1) is used. Defaults are seeded on first read so existing
// deployments don't change behaviour silently.
//
//   - CPUAllocationRatio (default 4.0): allowed sum-of-vCPU on a node
//     as a multiple of physical thread count. 4.0 lets you stack 32
//     vCPUs of declared capacity on an 8-thread host (typical homelab
//     density — most VMs idle far below their declared cores).
//   - RAMAllocationRatio (default 1.0): allowed committed RAM as a
//     multiple of physical RAM. 1.0 = no overcommit (Linux-host safe
//     default — RAM oversub trades free disk swap for RAM-pressure
//     surprises). Operators bump this to 1.2-1.5 only when their VMs
//     genuinely sit far below declared MaxMem.
//   - DiskAllocationRatio (default 1.0): allowed committed VM disk as
//     a multiple of pool capacity. 1.0 even though LVM-thin already
//     thin-provisions — the scheduler's job is to refuse placement
//     before the operator cuts off their own filesystem.
type SchedulingSettings struct {
	ID                  uint    `gorm:"primaryKey"`
	CPUAllocationRatio  float64 `gorm:"column:cpu_allocation_ratio;default:4.0"  json:"cpu_allocation_ratio"`
	RAMAllocationRatio  float64 `gorm:"column:ram_allocation_ratio;default:1.0"  json:"ram_allocation_ratio"`
	DiskAllocationRatio float64 `gorm:"column:disk_allocation_ratio;default:1.0" json:"disk_allocation_ratio"`
}

// GopherSettings stores the Gopher tunnel-gateway credentials. Only a single
// row (ID=1) is used. Empty APIURL means tunnel integration is disabled.
//
// CloudTunnel* fields hold the state of the Nimbus self-bootstrap — the
// reverse tunnel that exposes Nimbus's own dashboard at cloud.<domain>
// once Gopher is configured. Populated by selftunnel.Service in the
// background after the admin saves Gopher creds; the Settings UI polls
// CloudBootstrapState for progress.
type GopherSettings struct {
	ID     uint   `gorm:"primaryKey"`
	APIURL string `gorm:"default:''"`
	APIKey string `gorm:"default:''"`

	// CloudSubdomain is the leftmost label of the public hostname Nimbus's
	// self-tunnel exposes the dashboard at — e.g. "cloud" yields
	// cloud.<gopher-domain>. Empty is treated as the default "cloud" by
	// readers (selftunnel.EffectiveCloudSubdomain) so existing deployments
	// keep their current URL after the AutoMigrate adds this column.
	// Configurable so two Nimbus instances pointed at the same Gopher (e.g.
	// dev + prod) can each claim a distinct subdomain.
	CloudSubdomain string `gorm:"default:''"`

	// CloudMachineID is the Gopher ExternalMachine.ID for the Nimbus host
	// itself. Set as soon as POST /api/v1/machines returns; cleared on
	// teardown. Used for cleanup (DELETE /machines/:id) and for resuming
	// bootstrap when CloudTunnelID is empty but CloudMachineID is set.
	CloudMachineID string `gorm:"default:''"`
	// CloudTunnelID is the Gopher tunnel record exposing Nimbus's HTTP
	// port at cloud.<domain>. Set once the tunnel is active.
	CloudTunnelID string `gorm:"default:''"`
	// CloudTunnelURL is the public URL the Settings modal redirects to
	// once the tunnel is active (e.g., "https://cloud.altsuite.co").
	CloudTunnelURL string `gorm:"default:''"`
	// CloudBootstrapState is the self-bootstrap state machine:
	//   ""               — never attempted (or torn down)
	//   "registering"    — POST /machines in flight
	//   "installing"     — running curl|bash on the Nimbus host
	//   "waiting_connect"— machine registered, polling for status=connected
	//   "creating_tunnel"— machine connected, POST /tunnels in flight
	//   "active"         — tunnel up, CloudTunnelURL populated
	//   "failed"         — see CloudBootstrapError
	CloudBootstrapState string `gorm:"default:''"`
	// CloudBootstrapError carries the last failure reason. Cleared on a
	// successful run; surfaced verbatim in the Settings modal so the
	// operator can act on it.
	CloudBootstrapError string `gorm:"default:''"`
}

// GPUSettings stores the GX10 (or single-host GPU) plane configuration. Only
// a single row (ID=1) is used. When Enabled=false (or BaseURL empty), GPU
// integration is off — Nimbus injects no inference env vars into VMs and the
// jobs API rejects new submissions.
//
// WorkerToken is a pre-shared bearer the GX10's worker daemon presents on
// every /gpu/worker/* call. It's generated automatically when the GX10
// completes pairing (POST /api/gpu/register) — operators never see or
// touch it directly. Regenerating it cycles the credential without a
// Nimbus restart, but is rarely needed.
//
// PairingToken / PairingTokenExpiresAt back the "Add GX10" flow:
// admin mints a short-lived (5-min) pairing token, the install script on
// the GX10 trades it for a worker token via /api/gpu/register. Single use:
// register clears it. Empty PairingToken means no active pairing window.
type GPUSettings struct {
	ID                    uint       `gorm:"primaryKey"`
	Enabled               bool       `gorm:"default:false"`
	BaseURL               string     `gorm:"default:''"` // e.g. http://gx10.lan:8000
	InferenceModel        string     `gorm:"default:''"` // e.g. meta-llama/Llama-3.1-8B-Instruct
	WorkerToken           string     `gorm:"default:''"`
	GX10Hostname          string     `gorm:"default:''"` // self-reported hostname at registration time
	PairingToken          string     `gorm:"default:''"`
	PairingTokenExpiresAt *time.Time `gorm:"column:pairing_token_expires_at"`
}

// GPUJob is one queued / running / terminal training job submitted to the
// GPU plane. The queue is FIFO; ClaimNextJob's transactional UPDATE picks
// the oldest queued row and flips it to running.
//
// LogTail is an inline mirror of the last ~64 KB of the job's combined
// stdout+stderr — kept in the DB so the API can return it without disk I/O.
// The full log lives at /var/lib/nimbus/gpu-jobs/{ID}.log and is pruned by
// a startup sweep after 30 days.
//
// EnvJSON is a JSON-encoded map[string]string of additional env vars to
// inject into the docker run. The worker also adds a few baseline vars
// (HF_HOME, NIMBUS_GPU_API, etc.) that aren't stored here.
type GPUJob struct {
	gorm.Model
	OwnerID uint  `gorm:"column:owner_id;index;not null"  json:"owner_id"`
	VMID    *uint `gorm:"column:vm_id;index"              json:"vm_id,omitempty"`

	Status   string `gorm:"column:status;index;not null"   json:"status"` // queued|running|succeeded|failed|cancelled
	Image    string `gorm:"column:image;not null"          json:"image"`
	Command  string `gorm:"column:command;type:text"       json:"command"`
	EnvJSON  string `gorm:"column:env_json;type:text"      json:"env,omitempty"`
	WorkerID string `gorm:"column:worker_id"               json:"worker_id,omitempty"`

	// Terminal fields — populated only after the worker reports back.
	ExitCode     *int   `gorm:"column:exit_code"               json:"exit_code,omitempty"`
	ArtifactPath string `gorm:"column:artifact_path"           json:"artifact_path,omitempty"`
	ErrorMsg     string `gorm:"column:error_msg"               json:"error_msg,omitempty"`

	QueuedAt   time.Time  `gorm:"column:queued_at;index;not null"  json:"queued_at"`
	StartedAt  *time.Time `gorm:"column:started_at"                json:"started_at,omitempty"`
	FinishedAt *time.Time `gorm:"column:finished_at"               json:"finished_at,omitempty"`

	// LogTail caps at gpu.LogTailMax bytes. Older bytes are written to disk
	// only — readers that need full history must hit the on-disk file.
	LogTail string `gorm:"column:log_tail;type:text"        json:"log_tail,omitempty"`
}

// TableName pins the GORM table name so it isn't pluralized to "g_p_u_jobs"
// or similar by GORM's snake-case heuristic.
func (GPUJob) TableName() string { return "gpu_jobs" }

// VM is the canonical record for a provisioned virtual machine.
type VM struct {
	gorm.Model
	VMID     int    `gorm:"column:vmid;uniqueIndex;not null"        json:"vmid"`
	Hostname string `gorm:"column:hostname;uniqueIndex;not null"    json:"hostname"`
	IP       string `gorm:"column:ip;index;not null"                json:"ip"`
	Node     string `gorm:"column:node;not null"                    json:"node"`
	Tier     string `gorm:"column:tier;not null"                    json:"tier"`
	// RequiredTags is the host-aggregate filter the user opted into
	// at provision time, as a CSV string (e.g. "fast-cpu,nvme"). Used
	// by drain replacement to apply the same filter — a VM that
	// required `fast-cpu` only migrates to other `fast-cpu`-tagged
	// nodes. Empty = no constraint. Replaces the earlier (unmerged)
	// WorkloadType experiment; column name stays `workload_type` so
	// the schema doesn't require a rename migration on systems that
	// briefly ran the prior column.
	RequiredTags string `gorm:"column:workload_type;default:''"        json:"required_tags"`
	OSTemplate   string `gorm:"column:os_template;not null"            json:"os_template"`
	Username     string `gorm:"column:username"                         json:"username"`
	Status       string `gorm:"column:status;index;not null"            json:"status"`
	OwnerID      *uint  `gorm:"column:owner_id;index"                   json:"owner_id,omitempty"`
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
	// Tunnel fields (Phase 2 — Gopher integration). Empty when the user did
	// not request a public tunnel; TunnelError is non-empty when registration
	// or bootstrap failed but the VM itself is fine.
	TunnelID    string `gorm:"column:tunnel_id"                        json:"tunnel_id,omitempty"`
	TunnelURL   string `gorm:"column:tunnel_url"                       json:"tunnel_url,omitempty"`
	TunnelError string `gorm:"column:tunnel_error"                     json:"tunnel_error,omitempty"`
	ErrorMsg    string `gorm:"column:error_msg"                        json:"error_msg,omitempty"`
	// MissedCycles counts consecutive VM-reconciler runs in which Proxmox
	// reported no VM at this row's (node, vmid). Reset to 0 whenever the VM
	// is observed again. Crossing VACATE_MISS_THRESHOLD soft-deletes the row.
	// Default 0 — pre-existing rows behave correctly without backfill.
	MissedCycles int `gorm:"column:missed_cycles;default:0"          json:"missed_cycles,omitempty"`
	// SubnetID points to the user_subnets row this VM lives on. Nullable:
	// legacy VMs provisioned before SDN landed have NULL here and stay on
	// vmbr0 with the global IP pool; new SDN-provisioned VMs carry their
	// owning subnet's id. Cleared on subnet delete via FK rules in the
	// service layer (subnet delete refuses while any VM still references
	// it, so this stays non-NULL across the VM's life by construction).
	SubnetID *uint `gorm:"column:subnet_id;index"                   json:"subnet_id,omitempty"`
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
	// SystemGenerated marks keys auto-minted by Nimbus for internal VMs
	// (e.g. the S3 storage VM bootstrap) rather than created by users
	// through the Keys page. The Keys UI hides them by default behind a
	// toggle, and s3storage.Service.Delete garbage-collects them.
	SystemGenerated bool `gorm:"column:system_generated;index;default:false" json:"system_generated"`
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

// S3Storage is the singleton row describing the cluster's shared MinIO host.
// At most one row exists at a time (enforced in the s3storage service, not at
// the schema level). When absent, no shared object storage is deployed.
//
// Status values: "deploying", "ready", "error", "deleting".
//
// RootUser/RootPassword are the MinIO admin credentials Nimbus uses to manage
// buckets and service accounts. They are stored in plaintext for the hackathon
// MVP — fine for a self-hosted single-tenant box, not fine for anything else.
type S3Storage struct {
	gorm.Model
	// VMRowID is the Nimbus-side vms.id of the storage VM. Used by the
	// teardown path to call provision.Service.AdminDelete (which is keyed
	// on the Nimbus row id, not the Proxmox VMID). Nullable because a
	// crash mid-deploy can leave the row before Provision returns.
	VMRowID *uint `gorm:"column:vm_row_id;index"             json:"vm_row_id,omitempty"`
	// SSHKeyID points at the auto-generated SSH key created during the
	// deploy flow. Persisted by Deploy after Provision returns; consumed
	// by Service.Delete to garbage-collect the key when the storage VM is
	// torn down. Nullable for legacy rows from before the cleanup
	// landed — the startup backfill (cleanup.go) backfills the link.
	SSHKeyID     *uint  `gorm:"column:ssh_key_id;index"            json:"-"`
	VMID         int    `gorm:"column:vmid;uniqueIndex;not null"   json:"vmid"`
	Node         string `gorm:"column:node;not null"               json:"node"`
	IP           string `gorm:"column:ip"                          json:"ip,omitempty"`
	Status       string `gorm:"column:status;not null"             json:"status"`
	DiskGB       int    `gorm:"column:disk_gb;not null"            json:"disk_gb"`
	Endpoint     string `gorm:"column:endpoint"                    json:"endpoint,omitempty"`
	RootUser     string `gorm:"column:root_user"                   json:"-"`
	RootPassword string `gorm:"column:root_password"               json:"-"`
	ErrorMsg     string `gorm:"column:error_msg"                   json:"error_msg,omitempty"`
}

// TableName pins the table name; without this GORM would pluralize to "s3_storages".
func (S3Storage) TableName() string { return "s3_storage" }

// S3ServiceAccount is the per-user MinIO service account Nimbus mints lazily on
// the user's first /buckets visit (or first bucket-create). Each user gets one
// account; its policy is scoped to bucket names matching `<Prefix>-*`.
//
// SecretCT/SecretNonce hold the AES-256-GCM ciphertext of the MinIO secret
// key (encrypted via secrets.Cipher). The access key is stored plaintext —
// it appears in HTTP Authorization headers anyway and is paired with the
// secret in the same response only via an authenticated session.
//
// Prefix is computed once at mint time from the user's display name (ASCII-
// sanitized + `-u<id>` suffix) and is immutable thereafter — even if the
// user renames themselves, their bucket prefix and existing buckets stay
// valid.
type S3ServiceAccount struct {
	gorm.Model
	OwnerID     uint   `gorm:"column:owner_id;uniqueIndex;not null"   json:"owner_id"`
	Prefix      string `gorm:"column:prefix;uniqueIndex;not null"     json:"prefix"`
	AccessKey   string `gorm:"column:access_key;uniqueIndex;not null" json:"-"`
	SecretCT    []byte `gorm:"column:secret_ct"                       json:"-"`
	SecretNonce []byte `gorm:"column:secret_nonce"                    json:"-"`
}

// TableName pins the table name; without this GORM would pluralize.
func (S3ServiceAccount) TableName() string { return "s3_service_accounts" }

// S3Bucket is one user-owned bucket on the shared MinIO host. Name is the
// fully-composed `<owner-prefix>-<userPart>` form; the user never controls
// the prefix half. The DB is the source of truth for ownership — bucket
// listing in the UI filters by OwnerID, never by querying MinIO directly.
//
// Hard-deleted (along with S3ServiceAccount) when the storage VM is torn
// down (s3storage.Service.Delete) or when the owning user is deleted from
// Nimbus (s3storage.UserBucketService.PurgeForUser).
type S3Bucket struct {
	gorm.Model
	OwnerID uint   `gorm:"column:owner_id;index;not null"       json:"owner_id"`
	Name    string `gorm:"column:name;uniqueIndex;not null"     json:"name"`
}

// TableName pins the table name; without this GORM would pluralize to "s3_buckets" anyway,
// but pinning is consistent with the rest of the s3 tables.
func (S3Bucket) TableName() string { return "s3_buckets" }

// Node is the local cache row for one Proxmox cluster node. Holds operator-
// owned state (lock state, tags, lock-context metadata) that doesn't live in
// Proxmox itself. Live telemetry (CPU/RAM/status) is read straight from
// Proxmox at request time and not persisted here.
//
// LockState values:
//   - "none"      — normal; new VMs may land here per scoring
//   - "cordoned"  — scheduler skips this node; existing VMs untouched
//   - "draining"  — scheduler skips + a drain operation is migrating VMs off
//   - "drained"   — terminal; no managed VMs left, ready to remove from cluster
//
// Tags is a comma-separated string for forward-compat with workload-aware
// scoring (gpu, nvme-fast, arm64, …); empty for a fresh node. Mirrors the
// same CSV-in-a-string pattern NetworkSettings + GitHubOrgs use.
//
// Rows are populated lazily by nodemgr.Reconciler walking GetNodes — first
// observation auto-creates with LockState="none". Removed nodes are pruned
// when they go missing for VacateMissThreshold cycles, same logic the IP
// reconciler applies to allocations.
type Node struct {
	Name       string     `gorm:"column:name;primaryKey"             json:"name"`
	LockState  string     `gorm:"column:lock_state;default:'none'"   json:"lock_state"`
	LockedAt   *time.Time `gorm:"column:locked_at"                   json:"locked_at,omitempty"`
	LockedBy   *uint      `gorm:"column:locked_by"                   json:"locked_by,omitempty"`
	LockReason string     `gorm:"column:lock_reason;default:''"      json:"lock_reason,omitempty"`
	Tags       string     `gorm:"column:tags;default:''"             json:"-"`
	// CPUModel is denormalized from /nodes/{node}/status so the scheduler
	// can derive auto-tags (arch: x86 vs arm) without a per-call status
	// fan-out. Refreshed on every reconcileObserved cycle. Empty when the
	// status fan-out failed at the time of the last observation.
	CPUModel string `gorm:"column:cpu_model;default:''"        json:"-"`
	// HasSSD/HasGPU are auto-detected from /nodes/{n}/disks/list and
	// /nodes/{n}/hardware/pci respectively. Populated only by the
	// background reconcile loop (those endpoints aren't called on the
	// 15 s foreground polling cycle). False until the first reconcile
	// completes, which is fine — the auto-tags just don't appear yet.
	// HasGPU is currently NVIDIA-only (vendor 0x10de); AMD and Intel
	// discrete cards aren't reliably distinguishable from iGPUs via
	// the PCI vendor list and are left for operator tagging.
	HasSSD bool `gorm:"column:has_ssd;default:false"       json:"-"`
	HasGPU bool `gorm:"column:has_gpu;default:false"       json:"-"`
	// DiskType is the strongest disk class observed via /disks/list:
	// "nvme" > "ssd" > "hdd" > "" (unknown). Surfaces on the SPA card
	// in place of the Proxmox pool name so operators can compare
	// storage tiers at a glance. Distinct from has_ssd because the
	// auto-tag stays a binary union (nvme also flips ssd) — has_ssd
	// drives placement filtering, disk_type drives display.
	DiskType   string    `gorm:"column:disk_type;default:''"        json:"-"`
	LastSeenAt time.Time `gorm:"column:last_seen_at"                json:"last_seen_at"`
	CreatedAt  time.Time `gorm:"column:created_at"                  json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"                  json:"updated_at"`
}

// TableName pins the GORM table name. Without it GORM picks "nodes" which is
// fine — explicit just to match the rest of the file's pattern.
func (Node) TableName() string { return "nodes" }

// AuditEvent is one write-side action recorded for the Infrastructure →
// Audit log surface. Append-only: rows are inserted by every service
// that mutates state (provision, nodemgr, auth, settings) and pruned by
// a background reaper after NIMBUS_AUDIT_RETENTION_DAYS.
//
// Field meanings:
//   - Action is a dotted identifier ("vm.provision", "node.cordon",
//     "settings.smtp.update") so the SPA can group/filter by category.
//     Stable strings — operators write filter rules against them.
//   - ActorEmail is denormalized so a row survives user deletion. The
//     ActorID FK is also kept for joins when the user still exists.
//   - TargetType / TargetID / TargetLabel describe what was acted on.
//     TargetLabel is a human-readable name (hostname, node name, etc.)
//     so the table doesn't need to chase joins on every render.
//   - DetailsJSON is action-specific structured data: the request body
//     of a settings update, the migration plan of a drain, etc.
//     Always valid JSON or empty; readers tolerate either.
//   - Success + ErrorMsg let failed mutations be audited (a denied
//     login is more interesting than a successful one).
//   - IPAddress + RequestID make events traceable back to logs.
type AuditEvent struct {
	ID          uint      `gorm:"primaryKey"                              json:"id"`
	CreatedAt   time.Time `gorm:"column:created_at;index"                 json:"created_at"`
	ActorID     *uint     `gorm:"column:actor_id;index"                   json:"actor_id,omitempty"`
	ActorEmail  string    `gorm:"column:actor_email;default:''"           json:"actor_email,omitempty"`
	ActorAdmin  bool      `gorm:"column:actor_admin;default:false"        json:"actor_admin"`
	Action      string    `gorm:"column:action;index;not null"            json:"action"`
	TargetType  string    `gorm:"column:target_type;default:''"           json:"target_type,omitempty"`
	TargetID    string    `gorm:"column:target_id;default:''"             json:"target_id,omitempty"`
	TargetLabel string    `gorm:"column:target_label;default:''"          json:"target_label,omitempty"`
	IPAddress   string    `gorm:"column:ip_address;default:''"            json:"ip_address,omitempty"`
	RequestID   string    `gorm:"column:request_id;default:''"            json:"request_id,omitempty"`
	Success     bool      `gorm:"column:success"                          json:"success"`
	ErrorMsg    string    `gorm:"column:error_msg;default:''"             json:"error_msg,omitempty"`
	DetailsJSON string    `gorm:"column:details_json;default:''"          json:"details_json,omitempty"`
}

// TableName pins the GORM table to "audit_events". Without this GORM's
// pluralizer would produce "audit_events" anyway, but explicit is safer
// — readers query the table name directly during incident triage.
func (AuditEvent) TableName() string { return "audit_events" }

// NetworkSettings stores the runtime-editable IP pool range and gateway. Only a
// single row (ID=1) is used. The columns mirror the env vars they replace
// (IP_POOL_START / IP_POOL_END / GATEWAY_IP / VM_PREFIX_LEN) — env vars now
// act as first-boot defaults only; once the row is populated, the DB is the
// source of truth and admins manage these from Settings → Network.
//
// PrefixLen is the netmask length applied to every VM's cloud-init ipconfig0
// (e.g. 24 for /24, 16 for /16). Default 24 matches the historical
// hardcoded value, so existing deployments keep their current behaviour.
// Set to 16 (or whatever) when the cluster's VM bridge is on a larger
// network than a single /24.
type NetworkSettings struct {
	ID          uint   `gorm:"primaryKey"`
	IPPoolStart string `gorm:"default:''"`
	IPPoolEnd   string `gorm:"default:''"`
	GatewayIP   string `gorm:"default:''"`
	PrefixLen   int    `gorm:"default:24"`

	// SDN columns (P1 of per-user VNet isolation). Off by default —
	// existing deployments keep their flat-vmbr0 behaviour until an
	// admin opts in via Settings → Network. SDNZoneType today only
	// supports "simple"; the column exists so VXLAN can land in P4
	// without another migration.
	SDNEnabled        bool   `gorm:"column:sdn_enabled;default:false"`
	SDNZoneName       string `gorm:"column:sdn_zone_name;default:'nimbus'"`
	SDNZoneType       string `gorm:"column:sdn_zone_type;default:'simple'"`
	SDNSubnetSupernet string `gorm:"column:sdn_subnet_supernet;default:''"`
	// Subnet size carved per user (default /24, ~250 usable hosts).
	SDNSubnetSize int    `gorm:"column:sdn_subnet_size;default:24"`
	SDNDNSServer  string `gorm:"column:sdn_dns_server;default:''"`
}

// UserSubnet — one row per user-owned SDN subnet. Multiple subnets per
// user are allowed (OCI-style); each maps 1:1 to a Proxmox VNet + a
// single CIDR carved first-free from NetworkSettings.SDNSubnetSupernet.
//
// Each subnet is L2-isolated: a Proxmox VNet is its own broadcast
// domain with its own NAT gateway. So user A's "web-tier" subnet and
// user A's "db-tier" subnet cannot reach each other — VMs that need
// to talk must share a subnet. Mirrors OCI's empty-security-list
// default; routing between subnets is an explicit follow-up feature.
//
// Name + OwnerID are the user-facing identity (composite unique). VNet
// is the Proxmox-side identity (8-char limit, generated as
// `nbu<base36(ID)>` after row insert so we don't need a pre-allocated
// counter). IsDefault marks the subnet new VMs land on when the
// provision request doesn't pick one — exactly one default per user
// is enforced by the SetDefault path; first-time auto-create names it
// "default" and flips IsDefault=true.
type UserSubnet struct {
	gorm.Model
	OwnerID uint `gorm:"column:owner_id;not null;index;uniqueIndex:idx_owner_name"`
	// Name is the user-friendly label (DNS-label-shape). Unique
	// per-user; the composite index above enforces.
	Name string `gorm:"column:name;not null;uniqueIndex:idx_owner_name"`
	// VNet is the Proxmox VNet name — globally unique across the SDN
	// zone. 8-char max, lowercase alphanumeric, must start with a
	// letter. Generated as "nbu" + base36(ID).
	VNet string `gorm:"column:vnet;uniqueIndex;not null"`
	// Subnet is the CIDR (e.g. "10.42.1.0/24"). The supernet is
	// configured cluster-wide; per-subnet size is configurable
	// (default /24).
	Subnet string `gorm:"column:subnet;not null"`
	// Gateway is the .1 of Subnet — Proxmox's auto-NAT lives here.
	Gateway string `gorm:"column:gateway;not null"`
	// PoolStart / PoolEnd carve the usable host range out of Subnet
	// (typically .10..-.250 for /24). The IP pool reseeds against
	// these on subnet create + delete.
	PoolStart string `gorm:"column:pool_start;not null"`
	PoolEnd   string `gorm:"column:pool_end;not null"`
	// IsDefault marks the subnet new VMs land on when the provision
	// request doesn't pick a subnet.
	IsDefault bool `gorm:"column:is_default;default:false;index"`
	// Status reports operational state. "active" = ready; "error" =
	// Proxmox-side create failed mid-flight (admin can retry-or-delete).
	Status string `gorm:"column:status;not null;default:'active'"`
}
