package provision

// Quota defaults. Members hit these caps; admins bypass via Request.
// RequesterIsAdmin. They're vars (not consts) so tests can override; once a
// QuotaSettings table lands, swap these for a settings-backed lookup.
//
// MemberMaxVMs caps how many active VMs a single non-admin user can own
// concurrently. Beyond this, Provision returns ConflictError before any
// hypervisor work begins. Soft-deleted rows do not count.
//
// MemberAllowedTiers is the allowlist (not blocklist) of tiers non-admins
// may request. New tiers added to nodescore.Tiers are member-denied by
// default — the operator must explicitly opt them in here.
var (
	MemberMaxVMs       = 5
	MemberAllowedTiers = map[string]struct{}{
		"small":  {},
		"medium": {},
		"large":  {},
	}
)

// IsTierMemberAllowed reports whether the given tier is in the member
// allowlist. Admins should bypass this check via Request.RequesterIsAdmin.
func IsTierMemberAllowed(tier string) bool {
	_, ok := MemberAllowedTiers[tier]
	return ok
}
