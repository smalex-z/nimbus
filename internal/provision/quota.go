package provision

// MemberMaxVMs is the legacy fallback cap used when no QuotaResolver
// has been wired onto the Service. Tests rely on it being a var so
// they can override via the package-level binding without booting an
// auth service. Production wires service.AuthService through
// SetQuotaResolver and per-user overrides take precedence.
var MemberMaxVMs = 5

// MemberAllowedTiers is the allowlist (not blocklist) of tiers
// non-admins may request. New tiers added to nodescore.Tiers are
// member-denied by default — the operator must explicitly opt them in.
var MemberAllowedTiers = map[string]struct{}{
	"small":  {},
	"medium": {},
	"large":  {},
}

// QuotaResolver is the slice of service.AuthService the provision
// gate consults to find a user's effective VM cap (per-user override
// or workspace default). Defined here at the consumer per the
// "small interfaces, accept interfaces" convention; main.go wires
// AuthService in via Service.SetQuotaResolver. Nil falls back to
// the legacy MemberMaxVMs constant — keeps the test-only paths that
// don't construct a full auth stack working.
type QuotaResolver interface {
	EffectiveVMQuota(userID uint) (int, error)
}

// IsTierMemberAllowed reports whether the given tier is in the member
// allowlist. Admins should bypass this check via Request.RequesterIsAdmin.
func IsTierMemberAllowed(tier string) bool {
	_, ok := MemberAllowedTiers[tier]
	return ok
}
