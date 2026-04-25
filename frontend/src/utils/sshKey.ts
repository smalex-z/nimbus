// Cheap client-side sanity checks for SSH key uploads.
//
// Both validators return null on accept and an error string on reject. They
// only catch obvious wrong-file cases (a PNG, a private key dropped into the
// public-key slot, etc.). The backend is the source of truth and will reject
// anything subtly malformed regardless.

const PUBLIC_KEY_PREFIXES = [
  'ssh-rsa',
  'ssh-ed25519',
  'ssh-dss',
  'ecdsa-sha2-nistp256',
  'ecdsa-sha2-nistp384',
  'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com',
  'sk-ecdsa-sha2-nistp256@openssh.com',
]

export function validatePublicKey(text: string): string | null {
  const firstLine = text.trim().split(/\r?\n/, 1)[0] ?? ''
  const firstToken = firstLine.split(/\s+/, 1)[0] ?? ''
  if (!PUBLIC_KEY_PREFIXES.includes(firstToken)) {
    if (firstToken.startsWith('-----BEGIN')) {
      return "That looks like a private key, not a public key. Drop it in the Private key field instead."
    }
    return "Doesn't look like an SSH public key. Expected a line like ssh-ed25519 AAAA…"
  }
  return null
}

export function validatePrivateKey(text: string): string | null {
  const trimmed = text.trim()
  if (!trimmed.startsWith('-----BEGIN ')) {
    if (PUBLIC_KEY_PREFIXES.some((p) => trimmed.startsWith(p + ' '))) {
      return "That looks like a public key, not a private key. Drop it in the Public key field instead."
    }
    return "Doesn't look like a private key. Expected a PEM file starting with -----BEGIN …"
  }
  if (!/-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----/.test(trimmed)) {
    return "PEM block isn't a recognized private-key type."
  }
  return null
}
