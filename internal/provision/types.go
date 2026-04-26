package provision

// Request is a validated provisioning request submitted to the Service.
//
// SSH key resolution accepts (in priority order):
//
//  1. SSHKeyID — pick an existing row from the ssh_keys vault.
//  2. GenerateKey — mint a new keypair and persist it as a fresh ssh_keys row.
//  3. SSHPubKey (+ optional SSHPrivKey) — BYO; persisted as a new ssh_keys row.
//  4. None of the above — use the owner's default key, if one is set.
//
// The service is the place this gets resolved (the handler validates only
// shape, not key-store state).
type Request struct {
	Hostname    string
	Tier        string
	OSTemplate  string
	SSHKeyID    *uint  // optional: use an existing vault entry
	SSHPubKey   string // BYO
	SSHPrivKey  string // optional: BYO callers may stash the private half in Nimbus's vault
	GenerateKey bool
	OwnerID     *uint // nil in Phase 1 (no auth)

	// PublicTunnel asks Nimbus to register a Gopher tunnel for the new VM and
	// expose it at Subdomain.<gopher-zone>. Silently ignored when GOPHER_API_URL
	// is unset. TunnelPort is the in-VM target port Gopher should forward to;
	// 0 → 80 (the typical HTTP service port — Gopher does TLS termination).
	PublicTunnel bool
	Subdomain    string
	TunnelPort   int
}

// Result is the value returned to the user after a successful provision.
//
// SSHPrivateKey is populated only when GenerateKey was true on the request.
// It is never persisted and never logged — see (*Result).String for the
// redacted form used in log lines.
//
// Warning, when non-empty, indicates a "soft success": the VM was created
// and configured but Nimbus could not verify it was reachable on its
// assigned IP within the readiness budget. The credentials are still valid
// — they just couldn't be confirmed. Most common cause: Nimbus running
// outside the cluster's LAN, where the VM's internal IP isn't routable
// from Nimbus's network position.
type Result struct {
	// ID is the Nimbus DB row id, used by the result page to wire follow-up
	// actions (e.g. opening the per-VM tunnel manager) without a separate
	// lookup. Distinct from VMID, which is Proxmox's cluster-wide VMID.
	ID            uint   `json:"id"`
	VMID          int    `json:"vmid"`
	Hostname      string `json:"hostname"`
	IP            string `json:"ip"`
	Username      string `json:"username"`
	OS            string `json:"os"`
	Tier          string `json:"tier"`
	Node          string `json:"node"`
	SSHPrivateKey string `json:"ssh_private_key,omitempty"`
	KeyName       string `json:"key_name,omitempty"`
	Warning       string `json:"warning,omitempty"`

	// Tunnel fields. TunnelURL is set on success; TunnelError is populated
	// when registration or bootstrap fails but the VM is fine.
	TunnelURL   string `json:"tunnel_url,omitempty"`
	TunnelError string `json:"tunnel_error,omitempty"`
}

// String returns a log-safe representation of the Result that omits the
// private key entirely.
func (r *Result) String() string {
	if r == nil {
		return "<nil>"
	}
	return formatResult(r)
}

// Progress step IDs. The handler streams these to the frontend, which keys
// its checklist off them — keep the IDs stable.
const (
	StepReserveIP = "reserve_ip"
	StepCloneTpl  = "clone_template"
	StepConfigure = "configure_vm"
	StepStartVM   = "start_vm"
	StepWaitAgent = "wait_guest_agent"
)

// ProgressEvent marks completion of a phase. Emitted by Provision when the
// step finishes successfully — the next phase is implicitly "in progress"
// from the moment the previous one closes.
type ProgressEvent struct {
	Step  string `json:"step"`
	Label string `json:"label"`
}

// ProgressReporter is the optional callback the handler installs on a
// Provision call to receive ProgressEvents as steps complete. Nil is allowed
// — the service runs identically without one.
type ProgressReporter func(ProgressEvent)
