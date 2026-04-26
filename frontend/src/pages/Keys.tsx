import { useEffect, useState } from 'react'
import {
  attachPrivateKey,
  createKey,
  deleteKey,
  getKeyPrivate,
  listKeys,
  setDefaultKey,
} from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'
import Input from '@/components/ui/Input'
import KeyFileUpload from '@/components/ui/KeyFileUpload'
import RadioCard from '@/components/ui/RadioCard'
import Textarea from '@/components/ui/Textarea'
import { useAuth } from '@/hooks/useAuth'
import type { SSHKey } from '@/types'
import { validatePrivateKey, validatePublicKey } from '@/utils/sshKey'

type Mode = 'gen' | 'import'

export default function Keys() {
  const { user } = useAuth()
  const [keys, setKeys] = useState<SSHKey[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showAdd, setShowAdd] = useState(false)
  const [justCreated, setJustCreated] = useState<{ name: string; privateKey: string } | null>(null)

  const refresh = () => {
    setLoading(true)
    listKeys()
      .then(setKeys)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false))
  }

  useEffect(refresh, [])

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">{keys.length} key{keys.length === 1 ? '' : 's'}</div>
          <h2 className="text-3xl">SSH keys</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            Store keys once, use them on every VM. The default key is picked
            automatically at provision time.
          </p>
        </div>
        <Button onClick={() => setShowAdd((v) => !v)}>
          {showAdd ? '← Cancel' : '+ Add key'}
        </Button>
      </div>

      {showAdd && (
        <AddKeyForm
          onClose={() => setShowAdd(false)}
          onCreated={(name, privateKey) => {
            setShowAdd(false)
            if (privateKey) setJustCreated({ name, privateKey })
            refresh()
          }}
        />
      )}

      {justCreated && (
        <FreshlyGeneratedKey
          keyName={justCreated.name}
          privateKey={justCreated.privateKey}
          onDismiss={() => setJustCreated(null)}
        />
      )}

      {loading && <p className="mt-8 text-ink-3 font-mono text-sm">Loading…</p>}
      {error && (
        <Card className="mt-8 p-6 text-bad text-sm">Failed to load: {error}</Card>
      )}

      {!loading && !error && keys.length === 0 && !showAdd && (
        <Card className="mt-8 p-12 text-center">
          <div className="eyebrow">No keys yet</div>
          <h3 className="text-xl mt-2">Add your first SSH key.</h3>
          <p className="text-sm text-ink-2 mt-2">
            Generate one or paste in a key you already use.
          </p>
          <Button className="mt-5" onClick={() => setShowAdd(true)}>+ Add key</Button>
        </Card>
      )}

      <div className="grid gap-3 mt-7">
        {keys.map((k) => (
          <KeyRow
            key={k.id}
            sshKey={k}
            currentUserId={user?.id}
            onChanged={refresh}
            onPromoted={(id) =>
              setKeys((prev) => prev.map((row) => ({ ...row, is_default: row.id === id })))
            }
            onError={(msg) => setError(msg)}
          />
        ))}
      </div>
    </div>
  )
}

interface AddKeyFormProps {
  onClose: () => void
  onCreated: (name: string, privateKey: string | undefined) => void
}

function AddKeyForm({ onClose, onCreated }: AddKeyFormProps) {
  const [mode, setMode] = useState<Mode>('gen')
  const [name, setName] = useState('')
  const [label, setLabel] = useState('')
  const [pubKey, setPubKey] = useState('')
  const [privKey, setPrivKey] = useState('')
  const [setDefault, setSetDefault] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const canSubmit = (() => {
    if (!name.trim()) return false
    if (mode === 'import' && !pubKey.trim()) return false
    return true
  })()

  const submit = async () => {
    setErr(null)
    setSubmitting(true)
    try {
      const res = await createKey({
        name: name.trim(),
        label: label.trim() || undefined,
        public_key: mode === 'import' ? pubKey.trim() : undefined,
        private_key: mode === 'import' && privKey.trim() ? privKey.trim() : undefined,
        generate: mode === 'gen' ? true : undefined,
        set_default: setDefault || undefined,
      })
      onCreated(res.name, res.private_key)
    } catch (e: unknown) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Card className="mt-6 p-9">
      <div className="flex flex-col gap-6">
        <Input
          label="Name"
          placeholder="my-laptop"
          value={name}
          onChange={(e) => setName(e.target.value.toLowerCase())}
          hint="Lowercase letters, numbers, hyphens. Used as the on-disk filename when you download."
        />

        <Input
          label="Label (optional)"
          placeholder="MacBook · personal"
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          hint="Free-form display label. Won't affect the key file."
        />

        <div className="flex flex-col gap-2">
          <label className="text-[13px] font-medium text-ink">Source</label>
          <div className="grid gap-2">
            <RadioCard
              title="Generate one for me"
              description="We'll mint an Ed25519 keypair. The private half is shown once, vaulted, and re-downloadable."
              selected={mode === 'gen'}
              onClick={() => setMode('gen')}
            />
            <RadioCard
              title="Import an existing key"
              description="Paste a public key. Optionally include the private half so Nimbus can hand it back later."
              selected={mode === 'import'}
              onClick={() => setMode('import')}
            />
          </div>
        </div>

        {mode === 'import' && (
          <>
            <div className="flex flex-col gap-2">
              <label className="text-[13px] text-ink">
                <span className="font-semibold">Private key</span>{' '}
                <span className="text-ink-3 font-normal">(PEM or OpenSSH format) — optional</span>
              </label>
              <div className="flex items-stretch gap-3">
                <div className="flex-1 min-w-0">
                  <Textarea
                    monospace
                    placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…'}
                    value={privKey}
                    onChange={(e) => setPrivKey(e.target.value)}
                  />
                </div>
                <KeyFileUpload
                  maxBytes={64 * 1024}
                  sizeError="File too large — private keys are typically under 4 KB."
                  validate={validatePrivateKey}
                  onLoad={setPrivKey}
                />
              </div>
              <p className="text-[11px] text-ink-3">
                Stored encrypted. Nothing leaves Nimbus unless you ask.
              </p>
            </div>

            <div className="flex flex-col gap-2">
              <label className="text-[13px] text-ink">
                <span className="font-semibold">Public key</span>{' '}
                <span className="text-ink-3 font-normal">(authorized_keys format)</span>
              </label>
              <div className="flex items-stretch gap-3">
                <div className="flex-1 min-w-0">
                  <Textarea
                    monospace
                    placeholder="ssh-ed25519 AAAA..."
                    value={pubKey}
                    onChange={(e) => setPubKey(e.target.value)}
                  />
                </div>
                <KeyFileUpload
                  maxBytes={16 * 1024}
                  sizeError="File too large — public keys are typically under 1 KB."
                  validate={validatePublicKey}
                  onLoad={setPubKey}
                />
              </div>
            </div>
          </>
        )}

        <label className="flex items-center gap-3 cursor-pointer select-none">
          <input
            type="checkbox"
            checked={setDefault}
            onChange={(e) => setSetDefault(e.target.checked)}
            className="w-4 h-4 accent-ink"
          />
          <span className="text-sm text-ink">Set as default key</span>
        </label>

        {err && (
          <div className="p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
            {err}
          </div>
        )}

        <div className="flex gap-2.5 justify-end">
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={!canSubmit || submitting}>
            {submitting
              ? 'Saving…'
              : mode === 'gen'
                ? 'Generate key pair'
                : 'Save key pair'}
          </Button>
        </div>
      </div>
    </Card>
  )
}

interface FreshlyGeneratedKeyProps {
  keyName: string
  privateKey: string
  onDismiss: () => void
}

function FreshlyGeneratedKey({ keyName, privateKey, onDismiss }: FreshlyGeneratedKeyProps) {
  const download = () => {
    const content = privateKey.endsWith('\n') ? privateKey : privateKey + '\n'
    const blob = new Blob([content], { type: 'application/x-pem-file' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = keyName
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
  }

  return (
    <Card className="mt-6 p-7">
      <div className="eyebrow text-good">Generated</div>
      <h3 className="text-xl mt-1">{keyName} is ready.</h3>
      <p className="text-sm text-ink-2 mt-2">
        Download the private key now — you can also fetch it later from this page.
      </p>
      <div className="flex items-center gap-3 mt-5 flex-wrap">
        <button
          type="button"
          onClick={download}
          className="inline-flex items-center gap-2 px-4 py-2.5 rounded-[10px] bg-ink text-white font-mono text-xs tracking-wide hover:bg-ink-2 transition-colors"
        >
          <span aria-hidden>↓</span>
          <span>DOWNLOAD PRIVATE KEY</span>
        </button>
        <Button variant="ghost" size="small" onClick={onDismiss}>
          Dismiss
        </Button>
      </div>
    </Card>
  )
}

interface KeyRowProps {
  sshKey: SSHKey
  currentUserId: number | undefined
  onChanged: () => void
  onPromoted: (id: number) => void
  onError: (msg: string) => void
}

function KeyRow({ sshKey, currentUserId, onChanged, onPromoted, onError }: KeyRowProps) {
  const [busy, setBusy] = useState<null | 'default' | 'delete' | 'download'>(null)
  const [attaching, setAttaching] = useState(false)
  // Defense in depth: server-side filter already restricts the list to the
  // caller's own keys, but if the data is ever inconsistent, hide actions on
  // rows the user doesn't own.
  const isMine = currentUserId !== undefined && sshKey.owner_id === currentUserId

  const downloadKey = async () => {
    setBusy('download')
    try {
      const { key_name, private_key } = await getKeyPrivate(sshKey.id)
      const content = private_key.endsWith('\n') ? private_key : private_key + '\n'
      const blob = new Blob([content], { type: 'application/x-pem-file' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = key_name
      document.body.appendChild(a)
      a.click()
      document.body.removeChild(a)
      URL.revokeObjectURL(url)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const promote = async () => {
    setBusy('default')
    try {
      await setDefaultKey(sshKey.id)
      // Update the badge in place — the row will re-sort to the top on the
      // next page visit, but jumping mid-interaction is jarring.
      onPromoted(sshKey.id)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const remove = async () => {
    if (!window.confirm(`Delete key "${sshKey.name}"? VMs using it will keep working but Nimbus won't be able to hand back the private half.`)) {
      return
    }
    setBusy('delete')
    try {
      await deleteKey(sshKey.id)
      onChanged()
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  return (
    <Card className="p-5">
      <div className="grid grid-cols-1 sm:grid-cols-[1fr_auto] gap-5 items-start">
        <div className="min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-display text-lg font-medium">{sshKey.name}</span>
            {sshKey.is_default && (
              <span className="font-mono text-[10px] px-2 py-0.5 rounded-md bg-[rgba(45,125,90,0.1)] text-good uppercase tracking-wider">
                default
              </span>
            )}
            {sshKey.source === 'vm-auto' && (
              <span className="font-mono text-[10px] px-2 py-0.5 rounded-md bg-[rgba(27,23,38,0.05)] text-ink-2 uppercase tracking-wider">
                vm-auto
              </span>
            )}
            {!sshKey.has_private_key && (
              <span className="font-mono text-[10px] px-2 py-0.5 rounded-md bg-[rgba(184,101,15,0.1)] text-warn uppercase tracking-wider">
                public-only
              </span>
            )}
          </div>
          {sshKey.label && (
            <div className="text-sm text-ink-2 mt-1">{sshKey.label}</div>
          )}
          <div className="font-mono text-[11px] text-ink-3 mt-1.5 truncate" title={sshKey.fingerprint}>
            {sshKey.fingerprint || sshKey.public_key.slice(0, 64) + '…'}
          </div>
        </div>

        <div className="flex items-center gap-2 flex-wrap justify-end">
          <CopyButton value={sshKey.public_key} label="COPY PUB" />
          {isMine && sshKey.has_private_key && (
            <button
              type="button"
              onClick={downloadKey}
              disabled={busy !== null}
              className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors disabled:opacity-50"
            >
              <span aria-hidden>↓</span>
              <span>{busy === 'download' ? 'FETCHING…' : 'DOWNLOAD'}</span>
            </button>
          )}
          {isMine && !sshKey.has_private_key && (
            <button
              type="button"
              onClick={() => setAttaching((v) => !v)}
              disabled={busy !== null}
              className="inline-flex items-center px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors disabled:opacity-50"
            >
              {attaching ? 'CANCEL' : '+ PRIVATE KEY'}
            </button>
          )}
          {isMine && (
            <button
              type="button"
              onClick={sshKey.is_default ? undefined : promote}
              disabled={busy !== null}
              aria-pressed={sshKey.is_default}
              className={
                sshKey.is_default
                  ? 'inline-flex items-center gap-1 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-[rgba(45,125,90,0.3)] bg-[rgba(45,125,90,0.12)] text-good cursor-default'
                  : 'inline-flex items-center px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors disabled:opacity-50 cursor-pointer'
              }
            >
              {sshKey.is_default ? (
                <>
                  <span aria-hidden>✓</span>
                  <span>DEFAULT</span>
                </>
              ) : busy === 'default' ? (
                'SETTING…'
              ) : (
                'MAKE DEFAULT'
              )}
            </button>
          )}
          {isMine && (
            <button
              type="button"
              onClick={remove}
              disabled={busy !== null}
              className="inline-flex items-center px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-[rgba(184,58,58,0.25)] bg-[rgba(184,58,58,0.04)] text-bad hover:bg-[rgba(184,58,58,0.1)] transition-colors disabled:opacity-50"
            >
              {busy === 'delete' ? 'DELETING…' : 'DELETE'}
            </button>
          )}
        </div>
      </div>

      {attaching && !sshKey.has_private_key && (
        <AttachPrivateKeyForm
          keyId={sshKey.id}
          onClose={() => setAttaching(false)}
          onAttached={() => {
            setAttaching(false)
            onChanged()
          }}
          onError={onError}
        />
      )}
    </Card>
  )
}

interface AttachPrivateKeyFormProps {
  keyId: number
  onClose: () => void
  onAttached: () => void
  onError: (msg: string) => void
}

function AttachPrivateKeyForm({ keyId, onClose, onAttached, onError }: AttachPrivateKeyFormProps) {
  const [privKey, setPrivKey] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [localErr, setLocalErr] = useState<string | null>(null)

  const submit = async () => {
    const trimmed = privKey.trim()
    if (!trimmed) return
    setSubmitting(true)
    setLocalErr(null)
    try {
      await attachPrivateKey(keyId, trimmed)
      onAttached()
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      // Surface validation/conflict errors inline next to the form so the
      // user can correct without losing what they pasted; bubble unexpected
      // errors to the page-level banner.
      if (/match|already|private/i.test(msg)) {
        setLocalErr(msg)
      } else {
        onError(msg)
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="mt-5 pt-5 border-t border-dashed border-line">
      <label className="text-[13px] text-ink">
        <span className="font-semibold">Attach private key</span>{' '}
        <span className="text-ink-3 font-normal">(PEM or OpenSSH format) — must match the stored public key</span>
      </label>
      <div className="flex items-stretch gap-3 mt-2">
        <div className="flex-1 min-w-0">
          <Textarea
            monospace
            placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…'}
            value={privKey}
            onChange={(e) => setPrivKey(e.target.value)}
          />
        </div>
        <KeyFileUpload
          maxBytes={64 * 1024}
          sizeError="File too large — private keys are typically under 4 KB."
          validate={validatePrivateKey}
          onLoad={setPrivKey}
        />
      </div>
      {localErr && (
        <p className="text-[11px] text-bad mt-2">{localErr}</p>
      )}
      <div className="flex justify-end gap-2.5 mt-4">
        <Button variant="ghost" onClick={onClose} disabled={submitting}>
          Cancel
        </Button>
        <Button onClick={submit} disabled={!privKey.trim() || submitting}>
          {submitting ? 'Attaching…' : 'Attach'}
        </Button>
      </div>
    </div>
  )
}
