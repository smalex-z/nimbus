import { useEffect, useState } from 'react'
import { createVPC, deleteVPC, listVPCs } from '@/api/client'
import type { VPC } from '@/api/client'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'

// VPCs — Networking-v1 primitive. Each VPC is a VXLAN zone shared
// across cluster nodes (so VMs talk at L2 across nodes) plus a
// dedicated gateway LXC for outbound NAT egress. Users create VPCs
// here and pick one at VM provision time. CIDR is auto-allocated by
// the backend from the configured supernet — users don't pick.
export default function VPCs() {
  const [vpcs, setVPCs] = useState<VPC[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showAdd, setShowAdd] = useState(false)

  const refresh = () => {
    setLoading(true)
    listVPCs()
      .then(setVPCs)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false))
  }

  useEffect(refresh, [])

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">
            {vpcs.length} VPC{vpcs.length === 1 ? '' : 's'}
          </div>
          <h2 className="text-3xl">VPCs</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            A VPC is a private network that spans cluster nodes. VMs in
            the same VPC can talk to each other at L2 across nodes; VMs
            in different VPCs cannot. Each VPC gets its own NAT gateway
            LXC for outbound internet access.
          </p>
        </div>
        <Button onClick={() => setShowAdd((v) => !v)}>
          {showAdd ? '← Cancel' : '+ Create VPC'}
        </Button>
      </div>

      {showAdd && (
        <AddVPCForm
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

      {loading && vpcs.length === 0 ? (
        <p className="text-sm text-ink-3 mt-6">Loading…</p>
      ) : vpcs.length === 0 ? (
        <p className="text-sm text-ink-3 mt-6">
          No VPCs yet. Click <strong>+ Create VPC</strong> to make one —
          provisioning takes 30–60 seconds while the gateway LXC comes up.
        </p>
      ) : (
        <div className="mt-6 flex flex-col gap-3">
          {vpcs.map((v) => (
            <VPCRow key={v.id} vpc={v} onChanged={refresh} />
          ))}
        </div>
      )}
    </div>
  )
}

function AddVPCForm({
  onClose,
  onCreated,
}: {
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      await createVPC({ name: name.trim() })
      onCreated()
    } catch (e: unknown) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form
      onSubmit={submit}
      className="my-4 p-4 rounded-[10px] border border-stroke-1 bg-bg-2 flex flex-col gap-3"
    >
      <div>
        <label className="block text-sm text-ink-2 mb-1">Name</label>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. web-tier"
          required
          pattern="[a-z0-9][a-z0-9-]*"
          title="lowercase letters, digits, hyphens — must start with a letter or digit"
          autoFocus
        />
        <p className="mt-1 text-xs text-ink-3">
          1–32 chars, lowercase alphanumeric + hyphens.
        </p>
      </div>
      {err && (
        <p className="text-sm text-bad">{err}</p>
      )}
      <div className="flex gap-2 justify-end">
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy}>
          {busy ? 'Provisioning…' : 'Create VPC'}
        </Button>
      </div>
    </form>
  )
}

function VPCRow({ vpc, onChanged }: { vpc: VPC; onChanged: () => void }) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const handleDelete = async () => {
    if (
      !window.confirm(
        `Delete VPC "${vpc.name}"? This tears down the gateway LXC and ` +
          `releases the network. The VPC must have no member VMs.`,
      )
    ) {
      return
    }
    setBusy(true)
    setErr(null)
    try {
      await deleteVPC(vpc.id)
      onChanged()
    } catch (e: unknown) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="p-4 rounded-[10px] border border-stroke-1 bg-bg-1">
      <div className="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <div className="flex items-center gap-2">
            <span className="text-base font-medium">{vpc.name}</span>
            <StatusPill status={vpc.status} />
          </div>
          <div className="mt-1 text-sm text-ink-3 flex gap-3 flex-wrap">
            <span>
              <strong>CIDR:</strong> <code>{vpc.cidr}</code>
            </span>
            <span>
              <strong>Members:</strong> {vpc.member_count}
            </span>
            {vpc.gateway_node && (
              <span>
                <strong>Gateway:</strong> {vpc.gateway_node}
              </span>
            )}
          </div>
        </div>
        <Button variant="ghost" onClick={handleDelete} disabled={busy}>
          {busy ? 'Deleting…' : 'Delete'}
        </Button>
      </div>
      {err && <p className="mt-2 text-sm text-bad">{err}</p>}
    </div>
  )
}

function StatusPill({ status }: { status: VPC['status'] }) {
  const tone =
    status === 'active'
      ? 'bg-good/10 text-good border-good/30'
      : status === 'provisioning'
        ? 'bg-warn/10 text-warn border-warn/30'
        : 'bg-bad/10 text-bad border-bad/30'
  return (
    <span
      className={`text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded border ${tone}`}
    >
      {status}
    </span>
  )
}
