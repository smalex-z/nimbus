import { useEffect, useMemo, useState } from 'react'
import {
  forceGatewayUpdate,
  getNetworkSettings,
  getNetworkingInfo,
  getNetworkingV1Settings,
  listNodes,
  reconcileVMs,
  renumberAllVMs,
  saveNetworkSettings,
  saveNetworkingV1Settings,
} from '@/api/client'
import type {
  NetworkOpReport,
  NetworkSettingsView,
  NetworkingInfo,
  NetworkingV1Settings,
  VMReconcileReport,
} from '@/api/client'
import type { NodeView } from '@/types'

// Settings → Network page. Networking-v1: Standalone (per-VM Simple
// zone) is the always-on default; VPCs are opt-in via env vars; the
// Cluster LAN bridge is an admin escape hatch with an explicit
// member-allowed toggle. The form below covers what an admin can
// actually change at runtime — env-var-driven primitives are shown
// read-only with the env-var name so the operator knows where to go.

type DangerKind = 'renumber' | 'force-gateway'

export default function Network() {
  return (
    <div className="flex flex-col gap-8">
      <StandalonePanel />
      <VPCConfigPanel />
      <ClusterLANPanel />
      <PoolPanel />
      <DangerOpsPanel />
    </div>
  )
}

// StandalonePanel describes the always-on Standalone primitive.
// Read-only because the only knob (NIMBUS_STANDALONE_POOL_CIDR)
// changes IP carving math, which is a deployment-time decision.
function StandalonePanel() {
  const [info, setInfo] = useState<NetworkingInfo | null>(null)
  useEffect(() => {
    getNetworkingInfo().then(setInfo).catch(() => undefined)
  }, [])
  const enabled = info?.standalone_enabled ?? true
  return (
    <section>
      <div className="eyebrow">Networking v1</div>
      <h2 className="text-3xl">Standalone VMs</h2>
      <p className="text-base text-ink-2 mt-2 leading-relaxed max-w-2xl">
        Per-VM Simple zone with PVE-native SNAT. Each Standalone VM
        gets its own host-local /24 — no cross-VM communication. This
        is the default for new provisions and works on a fresh
        Nimbus with no setup.
      </p>
      <div className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">Status</span>
          <span
            className={`text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded border ${
              enabled ? 'bg-good/10 text-good border-good/30' : 'bg-bad/10 text-bad border-bad/30'
            }`}
          >
            {enabled ? 'available' : 'disabled'}
          </span>
        </div>
        <p className="mt-2 text-xs text-ink-3">
          IP supernet defaults to <code>10.128.0.0/9</code>. Override with{' '}
          <code className="font-mono">NIMBUS_STANDALONE_POOL_CIDR</code> if it
          conflicts with cluster-LAN routing.
        </p>
      </div>
    </section>
  )
}

// VPCConfigPanel is the live-editable VPC settings form. Configures
// network node (where every VPC's gateway LXC lives), the host-side
// IP pool the gateway LXCs allocate from, and an optional pinned
// LXC template (otherwise auto-picks Alpine 3.x).
function VPCConfigPanel() {
  const [settings, setSettings] = useState<NetworkingV1Settings | null>(null)
  const [nodes, setNodes] = useState<NodeView[]>([])
  const [networkNode, setNetworkNode] = useState('')
  const [ipPoolStart, setIPPoolStart] = useState('')
  const [ipPoolEnd, setIPPoolEnd] = useState('')
  const [template, setTemplate] = useState('')
  const [storage, setStorage] = useState('local-lvm')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    Promise.all([getNetworkingV1Settings(), listNodes()])
      .then(([s, ns]) => {
        setSettings(s)
        setNetworkNode(s.network_node)
        setIPPoolStart(s.lxc_ip_pool_start)
        setIPPoolEnd(s.lxc_ip_pool_end)
        setTemplate(s.lxc_template)
        setStorage(s.lxc_storage || 'local-lvm')
        setNodes(ns)
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
  }, [])

  const dirty = useMemo(() => {
    if (!settings) return false
    return (
      networkNode !== settings.network_node ||
      ipPoolStart !== settings.lxc_ip_pool_start ||
      ipPoolEnd !== settings.lxc_ip_pool_end ||
      template !== settings.lxc_template ||
      (storage || 'local-lvm') !== (settings.lxc_storage || 'local-lvm')
    )
  }, [settings, networkNode, ipPoolStart, ipPoolEnd, template, storage])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setError(null)
    setSaved(false)
    try {
      const next = await saveNetworkingV1Settings({
        network_node: networkNode.trim(),
        lxc_ip_pool_start: ipPoolStart.trim(),
        lxc_ip_pool_end: ipPoolEnd.trim(),
        lxc_template: template.trim(),
        lxc_storage: storage.trim() || 'local-lvm',
      })
      setSettings(next)
      setSaved(true)
      setTimeout(() => setSaved(false), 3000)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <section>
      <h2 className="text-3xl">VPCs</h2>
      <p className="text-base text-ink-2 mt-2 leading-relaxed max-w-2xl">
        VXLAN zone shared across cluster nodes plus a dedicated NAT
        gateway LXC per VPC. Pick a network node where every VPC's
        gateway LXC will live, and an IP range on that node's host
        network for the gateway eth0 side.
      </p>

      <form
        onSubmit={submit}
        className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85 flex flex-col gap-3"
      >
        <div>
          <div className="flex items-center gap-2 mb-2">
            <span className="text-sm font-medium">Status</span>
            <span
              className={`text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded border ${
                settings?.configured
                  ? 'bg-good/10 text-good border-good/30'
                  : 'bg-warn/10 text-warn border-warn/30'
              }`}
            >
              {settings?.configured ? 'configured' : 'needs setup'}
            </span>
          </div>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
          <Field label="Network node">
            <select
              value={networkNode}
              onChange={(e) => setNetworkNode(e.target.value)}
              className="n-input"
            >
              <option value="">— pick a node —</option>
              {nodes.map((n) => (
                <option key={n.name} value={n.name}>
                  {n.name}
                </option>
              ))}
            </select>
            <p className="mt-1 text-[11px] text-ink-3">
              Every VPC's gateway LXC lives here. v1 limitation; HA
              across nodes is a future phase.
            </p>
          </Field>

          <Field label="LXC storage">
            <input
              value={storage}
              onChange={(e) => setStorage(e.target.value)}
              placeholder="local-lvm"
              className="n-input font-mono"
            />
            <p className="mt-1 text-[11px] text-ink-3">
              Storage pool on the network node for the gateway LXC's rootfs.
            </p>
          </Field>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
          <Field label="Gateway-LXC IP pool start">
            <input
              value={ipPoolStart}
              onChange={(e) => setIPPoolStart(e.target.value)}
              placeholder="192.168.1.200"
              className="n-input font-mono"
            />
          </Field>
          <Field label="Gateway-LXC IP pool end">
            <input
              value={ipPoolEnd}
              onChange={(e) => setIPPoolEnd(e.target.value)}
              placeholder="192.168.1.250"
              className="n-input font-mono"
            />
          </Field>
        </div>
        <p className="-mt-1 text-[11px] text-ink-3">
          Host-network IPv4 range each VPC's gateway LXC eth0 allocates from.
          Pick a slice outside the LAN's DHCP range.
        </p>

        <Field label="LXC template (optional)">
          <input
            value={template}
            onChange={(e) => setTemplate(e.target.value)}
            placeholder="local:vztmpl/alpine-3.21-default_20241217_amd64.tar.xz"
            className="n-input font-mono"
          />
          <p className="mt-1 text-[11px] text-ink-3">
            Leave blank to auto-pick the latest Alpine 3.x system
            template via PVE's <code>aplinfo</code> repo (downloaded
            on first VPC create if not cached).
          </p>
        </Field>

        <div className="flex items-center gap-3 pt-1">
          <button
            type="submit"
            disabled={!dirty || saving}
            className="px-3.5 py-2 rounded-[8px] text-sm bg-ink text-white disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {saving ? 'Applying…' : 'Save & apply'}
          </button>
          {saved && <span className="text-xs text-good">Saved + applied.</span>}
          {error && <span className="text-xs text-bad">{error}</span>}
        </div>
        {saving && (
          <p className="text-[11px] text-ink-3">
            First-time apply may take ~30s while the Alpine template
            downloads to the network node.
          </p>
        )}
      </form>
    </section>
  )
}

// ClusterLANPanel — the toggle that decides whether non-admin users
// can pick the Cluster LAN escape hatch on the Provision page.
// Admins always see the chip; the toggle gates everyone else.
function ClusterLANPanel() {
  const [settings, setSettings] = useState<NetworkSettingsView | null>(null)
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    getNetworkSettings()
      .then(setSettings)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
  }, [])

  const toggle = async (next: boolean) => {
    if (!settings) return
    setBusy(true)
    setError(null)
    setSaved(false)
    try {
      const updated = await saveNetworkSettings({ cluster_lan_for_members: next })
      setSettings(updated)
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <h2 className="text-3xl">Cluster LAN access</h2>
      <p className="text-base text-ink-2 mt-2 leading-relaxed max-w-2xl">
        When enabled, non-admin users can pick "Cluster LAN" at provision
        time, attaching their VM directly to <code>vmbr0</code> with a
        global-pool IP. Admins always have this option regardless.
        Default off — most clusters want member VMs confined to
        Standalone or a VPC.
      </p>

      <div className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85">
        <label className="flex items-start gap-3 cursor-pointer">
          <input
            type="checkbox"
            checked={settings?.cluster_lan_for_members ?? false}
            onChange={(e) => toggle(e.target.checked)}
            disabled={busy || !settings}
            className="mt-0.5 w-4 h-4 accent-ink"
          />
          <div className="flex-1">
            <div className="text-sm font-medium">
              Allow members to attach VMs to <code>vmbr0</code>
            </div>
            <div className="text-xs text-ink-3 mt-0.5">
              Bypasses isolation. Useful for cluster-LAN management VMs
              when the operator trusts every member to do that
              responsibly.
            </div>
          </div>
        </label>
        {saved && (
          <p className="mt-2 text-xs text-good">Saved.</p>
        )}
        {error && (
          <p className="mt-2 text-xs text-bad">{error}</p>
        )}
      </div>
    </section>
  )
}

// PoolPanel covers the global IP pool — used by the Cluster LAN
// path (Bridge override) for VMs that aren't on Standalone or VPC.
// Standalone/VPC carve their own CIDRs and don't draw from this pool.
function PoolPanel() {
  const [settings, setSettings] = useState<NetworkSettingsView | null>(null)
  const [poolStart, setPoolStart] = useState('')
  const [poolEnd, setPoolEnd] = useState('')
  const [gateway, setGateway] = useState('')
  const [prefixLen, setPrefixLen] = useState(24)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    getNetworkSettings()
      .then((s) => {
        setSettings(s)
        setPoolStart(s.ip_pool_start)
        setPoolEnd(s.ip_pool_end)
        setGateway(s.gateway_ip)
        setPrefixLen(s.prefix_len > 0 ? s.prefix_len : 24)
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
  }, [])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setError(null)
    setSaved(false)
    try {
      const next = await saveNetworkSettings({
        ip_pool_start: poolStart,
        ip_pool_end: poolEnd,
        gateway_ip: gateway,
        prefix_len: prefixLen,
      })
      setSettings(next)
      setPoolStart(next.ip_pool_start)
      setPoolEnd(next.ip_pool_end)
      setGateway(next.gateway_ip)
      setPrefixLen(next.prefix_len > 0 ? next.prefix_len : 24)
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  const dirty =
    !!settings &&
    (poolStart !== settings.ip_pool_start ||
      poolEnd !== settings.ip_pool_end ||
      gateway !== settings.gateway_ip ||
      prefixLen !== settings.prefix_len)

  return (
    <section>
      <h2 className="text-3xl">Cluster LAN IP pool</h2>
      <p className="text-base text-ink-2 mt-2 leading-relaxed max-w-2xl">
        Range used when a VM is attached directly to <code>vmbr0</code>.
        Also drives the cloud-init defaults (gateway and prefix) for
        legacy VMs from before Networking v1. Standalone and VPC VMs
        carve their own CIDRs and do not draw from this pool.
      </p>

      <form
        onSubmit={handleSave}
        className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85 grid grid-cols-1 md:grid-cols-2 gap-3"
      >
        <Field label="IP pool start">
          <input
            value={poolStart}
            onChange={(e) => setPoolStart(e.target.value)}
            placeholder="192.168.1.100"
            className="n-input font-mono"
          />
        </Field>
        <Field label="IP pool end">
          <input
            value={poolEnd}
            onChange={(e) => setPoolEnd(e.target.value)}
            placeholder="192.168.1.200"
            className="n-input font-mono"
          />
        </Field>
        <Field label="Gateway IP">
          <input
            value={gateway}
            onChange={(e) => setGateway(e.target.value)}
            placeholder="192.168.1.1"
            className="n-input font-mono"
          />
        </Field>
        <Field label="Prefix length">
          <input
            type="number"
            value={prefixLen}
            min={1}
            max={32}
            onChange={(e) => setPrefixLen(Number(e.target.value))}
            className="n-input font-mono"
          />
        </Field>

        <div className="md:col-span-2 flex items-center gap-3 mt-2">
          <button
            type="submit"
            disabled={!dirty || saving}
            className="px-3.5 py-2 rounded-[8px] text-sm bg-ink text-white disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
          {saved && <span className="text-xs text-good">Saved.</span>}
          {error && <span className="text-xs text-bad">{error}</span>}
        </div>
      </form>
    </section>
  )
}

// DangerOpsPanel — disruptive batch ops on existing VMs. Renumber
// reassigns each VM a fresh IP; force-gateway pushes the saved
// gateway/prefix to every VM via cloud-init + reboot. Both reboot
// every VM in sequence — surface the warning and show the report.
function DangerOpsPanel() {
  const [danger, setDanger] = useState<DangerKind | null>(null)
  const [report, setReport] = useState<NetworkOpReport | null>(null)
  const [reportKind, setReportKind] = useState<DangerKind | null>(null)
  const [reconcileBusy, setReconcileBusy] = useState(false)
  const [reconcileReport, setReconcileReport] = useState<VMReconcileReport | null>(null)
  const [error, setError] = useState<string | null>(null)

  const runDanger = async (kind: DangerKind) => {
    setDanger(null)
    setError(null)
    setReport(null)
    try {
      const r = kind === 'renumber' ? await renumberAllVMs() : await forceGatewayUpdate()
      setReport(r)
      setReportKind(kind)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const runReconcile = async () => {
    setReconcileBusy(true)
    setError(null)
    setReconcileReport(null)
    try {
      const r = await reconcileVMs()
      setReconcileReport(r)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setReconcileBusy(false)
    }
  }

  return (
    <section>
      <h2 className="text-3xl">Maintenance</h2>
      <p className="text-base text-ink-2 mt-2 leading-relaxed max-w-2xl">
        Batch operations that touch every Nimbus-managed VM. Each one
        reboots the affected VMs serially — expect downtime
        proportional to fleet size.
      </p>

      <div className="mt-4 grid grid-cols-1 md:grid-cols-3 gap-3">
        <DangerCard
          title="Renumber all VMs"
          summary="Re-assigns each VM a fresh IP from the saved Cluster LAN pool, then reboots."
          buttonLabel="Renumber"
          onClick={() => setDanger('renumber')}
        />
        <DangerCard
          title="Push gateway to all VMs"
          summary="Re-stamps every VM's cloud-init ipconfig0 with the saved gateway and reboots them. Safe if VMs are on the same /24."
          buttonLabel="Push gateway"
          onClick={() => setDanger('force-gateway')}
        />
        <DangerCard
          title="Reconcile VM rows"
          summary="Walks the cluster, drops DB rows for VMs that no longer exist on Proxmox. Read-only on Proxmox."
          buttonLabel={reconcileBusy ? 'Reconciling…' : 'Reconcile'}
          onClick={runReconcile}
          disabled={reconcileBusy}
        />
      </div>

      {danger !== null && (
        <ConfirmModal
          kind={danger}
          onConfirm={() => runDanger(danger)}
          onCancel={() => setDanger(null)}
        />
      )}

      {error && (
        <div className="mt-4 p-3.5 rounded-[10px] bg-bad/10 border border-bad/30 text-bad text-sm">
          {error}
        </div>
      )}

      {report && reportKind && (
        <div className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85">
          <div className="text-sm font-medium">
            {reportKind === 'renumber' ? 'Renumber report' : 'Push-gateway report'}
          </div>
          <div className="mt-1 text-sm text-ink-3">
            Updated {report.updated} VM{report.updated === 1 ? '' : 's'};{' '}
            {report.failures.length} failure{report.failures.length === 1 ? '' : 's'}.
          </div>
          {report.failures.length > 0 && (
            <ul className="mt-2 text-xs text-bad font-mono flex flex-col gap-1">
              {report.failures.map((f) => (
                <li key={f.vmid}>
                  vmid={f.vmid} ({f.hostname}): {f.error}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {reconcileReport && (
        <div className="mt-4 p-4 rounded-[10px] border border-line-2 bg-white/85 text-sm">
          <div className="font-medium">Reconcile complete</div>
          <div className="mt-1 text-ink-3">
            {reconcileReport.deleted.length} deleted ·{' '}
            {reconcileReport.migrated.length} migrated ·{' '}
            {reconcileReport.renamed.length} renamed ·{' '}
            {reconcileReport.missed.length} still missing ·{' '}
            {reconcileReport.no_ops} no-op
          </div>
        </div>
      )}
    </section>
  )
}

function DangerCard({
  title,
  summary,
  buttonLabel,
  onClick,
  disabled,
}: {
  title: string
  summary: string
  buttonLabel: string
  onClick: () => void
  disabled?: boolean
}) {
  return (
    <div className="p-4 rounded-[10px] border border-line-2 bg-white/85 flex flex-col gap-2">
      <div className="text-sm font-medium">{title}</div>
      <p className="text-xs text-ink-3 leading-relaxed flex-1">{summary}</p>
      <button
        type="button"
        onClick={onClick}
        disabled={disabled}
        className="self-start px-3 py-1.5 rounded-[8px] text-xs border border-line-2 hover:border-ink/40 disabled:opacity-50"
      >
        {buttonLabel}
      </button>
    </div>
  )
}

function ConfirmModal({
  kind,
  onConfirm,
  onCancel,
}: {
  kind: DangerKind
  onConfirm: () => void
  onCancel: () => void
}) {
  const verb = kind === 'renumber' ? 'renumber every VM' : 'push the gateway to every VM'
  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-black/40">
      <div className="w-[420px] p-6 rounded-[14px] bg-white shadow-xl">
        <div className="text-base font-medium">Confirm {kind === 'renumber' ? 'renumber' : 'force-gateway'}</div>
        <p className="mt-2 text-sm text-ink-2">
          You're about to {verb}. Each VM reboots. Expect 30–90 seconds of downtime per VM.
        </p>
        <div className="mt-4 flex gap-2 justify-end">
          <button
            type="button"
            onClick={onCancel}
            className="px-3 py-1.5 rounded-[8px] text-sm border border-line-2"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={onConfirm}
            className="px-3 py-1.5 rounded-[8px] text-sm bg-bad text-white"
          >
            {kind === 'renumber' ? 'Renumber' : 'Push gateway'}
          </button>
        </div>
      </div>
    </div>
  )
}

function Field({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[11px] uppercase tracking-wider text-ink-3">{label}</span>
      {children}
    </label>
  )
}
