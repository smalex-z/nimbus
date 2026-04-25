package provision

// Request is a validated provisioning request submitted to the Service.
//
// Exactly one of SSHPubKey or GenerateKey must be set. The handler enforces
// this; the service trusts the input.
type Request struct {
	Hostname    string
	Tier        string
	OSTemplate  string
	SSHPubKey   string
	GenerateKey bool
	OwnerID     *uint // nil in Phase 1 (no auth)
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
}

// String returns a log-safe representation of the Result that omits the
// private key entirely.
func (r *Result) String() string {
	if r == nil {
		return "<nil>"
	}
	return formatResult(r)
}
