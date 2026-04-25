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
// Neither is persisted. The private key is shown to the user once and then
// dropped on the floor by design — Nimbus never sees it again.
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
