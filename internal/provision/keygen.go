package provision

import "nimbus/internal/sshkeys"

// GenerateEd25519 is a thin alias for sshkeys.GenerateEd25519. The keygen
// helpers used to live here; they were promoted to internal/sshkeys when the
// key vault was extracted into its own package. Kept exported for tests and
// callers that haven't moved yet.
func GenerateEd25519() (string, string, error) { return sshkeys.GenerateEd25519() }

// VerifyKeyPair is a thin alias for sshkeys.VerifyKeyPair. See GenerateEd25519.
func VerifyKeyPair(publicAuthorizedKey, privatePEM string) error {
	return sshkeys.VerifyKeyPair(publicAuthorizedKey, privatePEM)
}
