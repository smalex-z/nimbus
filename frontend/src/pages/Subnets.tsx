import { useEffect, useState } from 'react'
import {
  createSubnet,
  deleteSubnet,
  listSubnets,
  setDefaultSubnet,
} from '@/api/client'
import type { Subnet } from '@/api/client'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'

// Subnets — per-user SDN subnet management. OCI-style: users create
// subnets explicitly and pick one (or create new inline) at provision
// time. Each subnet maps to a Proxmox VNet with its own NAT'd L2
// segment, isolated from the cluster LAN AND from the user's other
// subnets (web-tier and db-tier can't reach each other unless the
// user puts the relevant VMs in the same subnet).
//
// Mirrors the SSH keys page: list, add inline, default chip, delete.
// The add form is intentionally minimal — name only. CIDR is carved
// first-free from the operator-configured supernet so users don't
// have to think about IP ranges.
export default function Subnets() {
  const [subnets, setSubnets] = useState<Subnet[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showAdd, setShowAdd] = useState(false)

  const refresh = () => {
    setLoading(true)
    listSubnets()
      .then(setSubnets)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false))
  }

  useEffect(refresh, [])

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">
            {subnets.length} subnet{subnets.length === 1 ? '' : 's'}
          </div>
          <h2 className="text-3xl">Subnets</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            Each subnet is its own isolated network. VMs within a subnet
            can talk to each other; VMs in different subnets — even your
            own — cannot. The default subnet is picked automatically at
            provision time when you don't choose one.
          </p>
        </div>
        <Button onClick={() => setShowAdd((v) => !v)}>
          {showAdd ? '← Cancel' : '+ Add subnet'}
        </Button>
      </div>

      {showAdd && (
        <AddSubnetForm
          onClose={() => setShowAdd(false)}
          onCreated={() => {
            setShowAdd(false)
            refresh()
          }}
        />
      )}

      {error && (
        <div className="my-4 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
          {error}
        </div>
      )}

      {loading && subnets.length === 0 ? (
        <p className="text-sm text-ink-3 mt-6">Loading…</p>
      ) : subnets.length === 0 ? (
        <p className="text-sm text-ink-3 mt-6">
          No subnets yet. Click <strong>+ Add subnet</strong> to create
          your first one — or just provision a VM and we'll lazily make
          a <code>default</code> subnet for you.
        </p>
      ) : (
        <div className="mt-6 flex flex-col gap-3">
          {subnets.map((s) => (
            <SubnetRow key={s.id} subnet={s} onChanged={refresh} />
          ))}
        </div>
      )}
    </div>
  )
}

function AddSubnetForm({
  onClose,
  onCreated,
}: {
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [setDefault, setSetDefault] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await createSubnet({ name: name.trim(), set_default: setDefault })
      onCreated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'create failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <form
      onSubmit={submit}
      className="glass mt-6 p-6"
      style={{ display: 'flex', flexDirection: 'column', gap: 14 }}
    >
      <div>
        <div className="eyebrow">New subnet</div>
        <h3 className="text-xl mt-1">Name and create</h3>
      </div>
      <div className="n-field">
        <label className="n-label" htmlFor="subnet-name">Name</label>
        <Input
          id="subnet-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="web-tier"
          disabled={busy}
          autoFocus
          maxLength={32}
        />
        <p className="text-[11px] text-ink-3 mt-1">
          Lowercase letters/digits/hyphens, 1–32 chars, must start with
          a letter. The CIDR + gateway are picked for you.
        </p>
      </div>
      <label
        className="flex items-center gap-2 text-sm text-ink-2"
        style={{ userSelect: 'none' }}
      >
        <input
          type="checkbox"
          checked={setDefault}
          onChange={(e) => setSetDefault(e.target.checked)}
          disabled={busy}
        />
        Make this my default
      </label>
      {error && (
        <p className="text-sm text-bad">{error}</p>
      )}
      <div className="flex gap-3 justify-end">
        <Button variant="ghost" type="button" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || name.trim() === ''}>
          {busy ? 'Creating…' : 'Create subnet'}
        </Button>
      </div>
    </form>
  )
}

function SubnetRow({ subnet, onChanged }: { subnet: Subnet; onChanged: () => void }) {
  const [busy, setBusy] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const handleSetDefault = async () => {
    setBusy(true)
    setError(null)
    try {
      await setDefaultSubnet(subnet.id)
      onChanged()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  const handleDelete = async () => {
    setBusy(true)
    setError(null)
    try {
      await deleteSubnet(subnet.id)
      onChanged()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'delete failed')
      setConfirmDelete(false)
    } finally {
      setBusy(false)
    }
  }

  const statusChip = subnet.status === 'error' ? (
    <span className="font-mono text-[10px] uppercase tracking-widest text-bad bg-[rgba(184,58,58,0.12)] border border-[rgba(184,58,58,0.25)] px-1.5 py-px rounded ml-2">
      error
    </span>
  ) : null

  return (
    <div className="glass p-5 flex items-center gap-4 flex-wrap">
      <div className="flex-1 min-w-[200px]">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-base font-medium">{subnet.name}</span>
          {subnet.is_default && (
            <span className="font-mono text-[10px] uppercase tracking-widest text-good bg-[rgba(48,128,72,0.12)] border border-[rgba(48,128,72,0.25)] px-1.5 py-px rounded">
              default
            </span>
          )}
          {statusChip}
        </div>
        <div className="font-mono text-[11px] text-ink-3 mt-1">
          {subnet.subnet} · gateway {subnet.gateway} · vnet {subnet.vnet}
        </div>
      </div>
      <div className="flex gap-2 items-center">
        {!subnet.is_default && (
          <Button variant="ghost" size="small" onClick={handleSetDefault} disabled={busy}>
            Make default
          </Button>
        )}
        {confirmDelete ? (
          <>
            <span className="text-xs text-bad">Sure?</span>
            <Button variant="ghost" size="small" onClick={() => setConfirmDelete(false)} disabled={busy}>
              Cancel
            </Button>
            <Button variant="danger" size="small" onClick={handleDelete} disabled={busy}>
              {busy ? 'Deleting…' : 'Delete'}
            </Button>
          </>
        ) : (
          <Button variant="danger" size="small" onClick={() => setConfirmDelete(true)} disabled={busy}>
            Delete
          </Button>
        )}
      </div>
      {error && (
        <p className="text-xs text-bad w-full">{error}</p>
      )}
    </div>
  )
}
