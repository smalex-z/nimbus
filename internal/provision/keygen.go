package provision

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// GenerateEd25519 mints a fresh Ed25519 keypair and returns:
//   - publicAuthorizedKey: a single line in OpenSSH "ssh-ed25519 base64..." form
//     suitable for an authorized_keys file or Proxmox's `sshkeys` field.
//   - privatePEM: the OpenSSH-format private key, multi-line PEM, suitable for
//     handing to a user to drop into ~/.ssh.
//
// The caller decides what to do with each — the service layer encrypts and
// persists the private half in the key vault and returns it to the user once
// in the API response.
func GenerateEd25519() (publicAuthorizedKey string, privatePEM string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("encode ssh public key: %w", err)
	}
	publicAuthorizedKey = string(ssh.MarshalAuthorizedKey(sshPub))

	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", fmt.Errorf("marshal openssh private key: %w", err)
	}
	privatePEM = string(pem.EncodeToMemory(pemBlock))
	return publicAuthorizedKey, privatePEM, nil
}

// VerifyKeyPair returns nil if privatePEM and publicAuthorizedKey are halves
// of the same SSH keypair, otherwise an error describing the mismatch.
//
// Used to reject a BYO private key that doesn't correspond to the supplied
// public key before we persist either — otherwise the user could store a key
// pair that won't actually open the VM.
func VerifyKeyPair(publicAuthorizedKey, privatePEM string) error {
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicAuthorizedKey))
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}
	derived := signer.PublicKey()
	if derived.Type() != pub.Type() ||
		string(derived.Marshal()) != string(pub.Marshal()) {
		return fmt.Errorf("public key does not match private key")
	}
	return nil
}
