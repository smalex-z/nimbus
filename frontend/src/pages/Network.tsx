import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  forceGatewayUpdate,
  getNetworkSettings,
  getSDNSettings,
  getTunnelInfo,
  reconcileVMs,
  renumberAllVMs,
  saveNetworkSettings,
  saveSDNSettings,
} from '@/api/client'
import type {
  NetworkOpReport,
  NetworkSettingsView,
  SDNSettingsView,
  TunnelInfo,
  VMReconcileReport,
} from '@/api/client'

type DangerKind = 'renumber' | 'force-gateway'

function NetworkPanel() {
  const [settings, setSettings] = useState<NetworkSettingsView | null>(null)
  const [poolStart, setPoolStart] = useState('')
  const [poolEnd, setPoolEnd] = useState('')
  const [gateway, setGateway] = useState('')
  const [prefixLen, setPrefixLen] = useState(24)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [danger, setDanger] = useState<DangerKind | null>(null)
  const [report, setReport] = useState<NetworkOpReport | null>(null)
  const [reportKind, setReportKind] = useState<DangerKind | null>(null)

  useEffect(() => {
    getNetworkSettings()
      .then((s) => {
        setSettings(s)
        setPoolStart(s.ip_pool_start)
        setPoolEnd(s.ip_pool_end)
        setGateway(s.gateway_ip)
        setPrefixLen(s.prefix_len > 0 ? s.prefix_len : 24)
      })
      .catch(() => setError('Failed to load network settings'))
  }, [])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    setReport(null)
    try {
      setSaving(true)
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
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const dirty =
    !!settings &&
    (poolStart !== settings.ip_pool_start ||
      poolEnd !== settings.ip_pool_end ||
      gateway !== settings.gateway_ip ||
      prefixLen !== (settings.prefix_len > 0 ? settings.prefix_len : 24))

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          IP pool & gateway
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Saved values take effect for all <em>new</em> VMs immediately. Existing
        VMs keep their current IP and gateway until you explicitly push the
        change to them — see the action buttons below the form. Changing the
        gateway in your network without also pushing it to existing VMs will
        cut them off from the LAN.
      </p>

      <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="n-field">
          <label className="n-label" htmlFor="net-pool-start">IP pool start</label>
          <input
            id="net-pool-start"
            className="n-input"
            type="text"
            placeholder="192.168.0.150"
            value={poolStart}
            onChange={(e) => setPoolStart(e.target.value)}
          />
        </div>
        <div className="n-field">
          <label className="n-label" htmlFor="net-pool-end">IP pool end</label>
          <input
            id="net-pool-end"
            className="n-input"
            type="text"
            placeholder="192.168.0.200"
            value={poolEnd}
            onChange={(e) => setPoolEnd(e.target.value)}
          />
        </div>
        <div className="n-field">
          <label className="n-label" htmlFor="net-gateway">Gateway IP / subnet prefix</label>
          <div style={{ display: 'flex', alignItems: 'stretch', gap: 6 }}>
            <input
              id="net-gateway"
              className="n-input"
              type="text"
              placeholder="192.168.0.1"
              value={gateway}
              onChange={(e) => setGateway(e.target.value)}
              style={{ flex: 1 }}
            />
            <span
              aria-hidden="true"
              style={{
                display: 'flex',
                alignItems: 'center',
                padding: '0 6px',
                color: 'var(--ink-mute)',
                fontSize: 16,
                fontFamily: 'var(--font-mono, monospace)',
              }}
            >
              /
            </span>
            <input
              id="net-prefix"
              className="n-input"
              type="number"
              min={1}
              max={32}
              placeholder="24"
              value={prefixLen}
              onChange={(e) => setPrefixLen(Number(e.target.value))}
              aria-label="Subnet prefix length"
              style={{ width: 88, textAlign: 'center' }}
            />
          </div>
          <span style={{ fontSize: 12, color: 'var(--ink-mute)', marginTop: 4 }}>
            Default route + CIDR netmask Nimbus stamps into every VM's cloud-init (e.g. <code>192.168.0.1 / 24</code>, <code>10.0.0.1 / 16</code>). Applies to new VMs only — existing VMs keep their config until you push it with the action below.
          </span>
        </div>

        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}

        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button
            type="submit"
            className="n-btn n-btn-primary"
            disabled={saving || !dirty}
            style={{ minWidth: 100 }}
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
          {saved && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
          {!dirty && !saving && !saved && (
            <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
              No unsaved changes.
            </span>
          )}
        </div>
      </form>

      <div
        style={{
          marginTop: 6,
          paddingTop: 18,
          borderTop: '1px solid var(--line)',
          display: 'flex',
          flexDirection: 'column',
          gap: 14,
        }}
      >
        <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
          Apply to existing VMs
        </div>
        <p style={{ margin: 0, fontSize: 12.5, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Both actions reboot every running nimbus-managed VM. Sessions drop,
          in-progress work in user shells dies. Use only after you've saved
          the new values above.
        </p>
        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
          <button
            type="button"
            className="n-btn n-btn-secondary"
            disabled={dirty}
            onClick={() => {
              setReport(null)
              setReportKind(null)
              setDanger('force-gateway')
            }}
            title={dirty ? 'Save your changes first' : ''}
          >
            Force gateway + subnet on every VM
          </button>
          <button
            type="button"
            className="n-btn n-btn-secondary"
            disabled={dirty}
            onClick={() => {
              setReport(null)
              setReportKind(null)
              setDanger('renumber')
            }}
            title={dirty ? 'Save your changes first' : ''}
          >
            Renumber every VM into new pool
          </button>
        </div>
      </div>

      {report && (
        <div
          style={{
            marginTop: 4,
            padding: '14px 16px',
            border: '1px solid var(--line)',
            borderRadius: 8,
            background: 'rgba(20,18,28,0.03)',
            display: 'flex',
            flexDirection: 'column',
            gap: 8,
          }}
        >
          <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
            {reportKind === 'renumber' ? 'Renumber complete' : 'Network config push complete'}
          </div>
          <div style={{ fontSize: 13, color: 'var(--ink-body)' }}>
            {report.updated} VM{report.updated === 1 ? '' : 's'} updated.{' '}
            {report.failures.length > 0
              ? `${report.failures.length} failure${report.failures.length === 1 ? '' : 's'}:`
              : 'No failures.'}
          </div>
          {report.failures.length > 0 && (
            <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12.5, color: 'var(--err)' }}>
              {report.failures.map((f) => (
                <li key={f.vm_row_id}>
                  {f.hostname || `vm row ${f.vm_row_id}`} (vmid {f.vmid}): {f.error}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {danger && (
        <DangerModal
          kind={danger}
          settings={settings}
          onClose={() => setDanger(null)}
          onDone={(rep, kind) => {
            setReport(rep)
            setReportKind(kind)
            setDanger(null)
          }}
        />
      )}
    </div>
  )
}

function DangerModal({
  kind,
  settings,
  onClose,
  onDone,
}: {
  kind: DangerKind
  settings: NetworkSettingsView | null
  onClose: () => void
  onDone: (rep: NetworkOpReport, kind: DangerKind) => void
}) {
  const required =
    kind === 'renumber' ? 'RENUMBER ALL VMS' : 'CHANGE NETWORK ON ALL VMS'
  const [typed, setTyped] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const title =
    kind === 'renumber'
      ? 'Renumber every nimbus-managed VM'
      : 'Force gateway + subnet on every VM'

  const description =
    kind === 'renumber'
      ? `Every nimbus-managed VM will be assigned a fresh IP from ${
          settings?.ip_pool_start ?? '?'
        } – ${settings?.ip_pool_end ?? '?'} and rebooted. The new pool must have at least as many free addresses as you have VMs. Any open SSH sessions will drop. This cannot be cleanly undone — the old IPs are released back to the pool.`
      : `Every nimbus-managed VM will be reconfigured to use ${
          settings?.gateway_ip ?? '?'
        } as its default gateway with a /${
          settings?.prefix_len && settings.prefix_len > 0 ? settings.prefix_len : 24
        } subnet, then rebooted. If the new gateway is not yet reachable on your network, every VM will lose connectivity until the network is fixed. Any open SSH sessions will drop.`

  const handleConfirm = async () => {
    setError(null)
    try {
      setRunning(true)
      const rep = kind === 'renumber' ? await renumberAllVMs() : await forceGatewayUpdate()
      onDone(rep, kind)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Operation failed')
    } finally {
      setRunning(false)
    }
  }

  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(8,6,12,0.55)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 50,
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="glass"
        style={{
          width: 'min(520px, 92vw)',
          padding: '22px 24px',
          display: 'flex',
          flexDirection: 'column',
          gap: 14,
          borderColor: 'var(--err)',
        }}
      >
        <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--err)' }}>
          {title}
        </div>
        <p style={{ margin: 0, fontSize: 13.5, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          {description}
        </p>
        <div className="n-field">
          <label className="n-label" htmlFor="danger-confirm">
            Type <code style={{ color: 'var(--err)' }}>{required}</code> to confirm
          </label>
          <input
            id="danger-confirm"
            className="n-input"
            type="text"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            placeholder={required}
            autoFocus
          />
        </div>
        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
        <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
          <button type="button" className="n-btn" onClick={onClose} disabled={running}>
            Cancel
          </button>
          <button
            type="button"
            className="n-btn n-btn-primary"
            onClick={handleConfirm}
            disabled={running || typed !== required}
            style={{ background: 'var(--err)', borderColor: 'var(--err)' }}
          >
            {running ? 'Working…' : kind === 'renumber' ? 'Renumber all VMs' : 'Force gateway + subnet'}
          </button>
        </div>
      </div>
    </div>
  )
}

function SyncPanel() {
  const [running, setRunning] = useState(false)
  const [report, setReport] = useState<VMReconcileReport | null>(null)
  const [error, setError] = useState<string | null>(null)

  const handleSync = async () => {
    setError(null)
    setReport(null)
    try {
      setRunning(true)
      const rep = await reconcileVMs()
      setReport(rep)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Reconcile failed')
    } finally {
      setRunning(false)
    }
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}
    >
      <div style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Sync DB with cluster
      </div>
      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Walks every VM row against the live Proxmox cluster. Rows whose VM
        moved to a different node (manual <code>qm migrate</code>) get their
        node updated. Rows whose Proxmox display name disagrees with the
        local hostname get renamed — Proxmox is the source of truth.
        Rows whose VMID hasn't been observed for 3 consecutive runs get
        soft-deleted — typically because someone destroyed the VM directly
        through Proxmox, leaving an orphan here. This runs on the background
        reconcile loop every minute; the button forces an immediate pass.
      </p>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          type="button"
          className="n-btn n-btn-primary"
          onClick={handleSync}
          disabled={running}
          style={{ minWidth: 160 }}
        >
          {running ? 'Syncing…' : 'Sync now'}
        </button>
        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
      </div>
      {report && (
        <div
          style={{
            marginTop: 4,
            padding: '14px 16px',
            border: '1px solid var(--line)',
            borderRadius: 8,
            background: 'rgba(20,18,28,0.03)',
            display: 'flex',
            flexDirection: 'column',
            gap: 8,
            fontSize: 13,
            color: 'var(--ink-body)',
          }}
        >
          <div>
            Migrated: <strong>{report.migrated.length}</strong> &middot; Renamed:{' '}
            <strong>{report.renamed.length}</strong> &middot; Soft-deleted:{' '}
            <strong>{report.deleted.length}</strong> &middot; Going stale:{' '}
            <strong>{report.missed.length}</strong> &middot; In sync:{' '}
            <strong>{report.no_ops}</strong>
          </div>
          {report.migrated.length > 0 && (
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {report.migrated.map((m) => (
                <li key={`mig-${m.vm_row_id}`}>
                  {m.hostname} (vmid {m.vmid}): {m.from_node} → {m.to_node}
                </li>
              ))}
            </ul>
          )}
          {report.renamed.length > 0 && (
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {report.renamed.map((r) => (
                <li key={`ren-${r.vm_row_id}`}>
                  vmid {r.vmid} on {r.node}: {r.from_name} → {r.to_name}
                </li>
              ))}
            </ul>
          )}
          {report.deleted.length > 0 && (
            <ul style={{ margin: 0, paddingLeft: 18, color: 'var(--err)' }}>
              {report.deleted.map((d) => (
                <li key={`del-${d.vm_row_id}`}>
                  {d.hostname} (vmid {d.vmid} on {d.node}) — soft-deleted
                </li>
              ))}
            </ul>
          )}
          {report.missed.length > 0 && (
            <ul style={{ margin: 0, paddingLeft: 18, color: 'var(--ink-mute)' }}>
              {report.missed.map((m) => (
                <li key={`miss-${m.vm_row_id}`}>
                  {m.hostname} (vmid {m.vmid} on {m.node}) — missed{' '}
                  {m.missed_cycles}/3
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

export default function Network() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          VM network
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          The IP pool nimbus draws from when provisioning, and the gateway
          handed to each VM via cloud-init.
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          <NetworkPanel />
          <SDNPanel />
          <SyncPanel />
        </div>
      </div>
    </div>
  )
}

// SDNPanel — admin toggle + diagnostic for the per-user SDN VNet
// isolation feature. Default off: turning isolation on means VMs are
// unreachable from anywhere except the cluster's PVE host until at
// least one user-facing connect path (Gopher tunnel today, browser
// SSH next) is wired up. The reachability callout below makes that
// explicit so admins don't ship broken connect UX to their users.
function SDNPanel() {
  const [view, setView] = useState<SDNSettingsView | null>(null)
  const [enabled, setEnabled] = useState(false)
  const [zoneName, setZoneName] = useState('nimbus')
  const [supernet, setSupernet] = useState('')
  const [subnetSize, setSubnetSize] = useState(24)
  const [dnsServer, setDNSServer] = useState('')
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [tunnelInfo, setTunnelInfo] = useState<TunnelInfo | null>(null)

  const load = () => {
    getSDNSettings()
      .then((v) => {
        setView(v)
        setEnabled(v.enabled)
        setZoneName(v.zone_name || 'nimbus')
        setSupernet(v.supernet)
        setSubnetSize(v.subnet_size > 0 ? v.subnet_size : 24)
        setDNSServer(v.dns_server || '')
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : 'failed to load SDN settings'),
      )
    getTunnelInfo()
      .then(setTunnelInfo)
      .catch(() => {
        // Tunnel-info fetch failures are non-fatal — the reachability
        // callout falls back to "not configured" if it can't read.
      })
  }
  useEffect(load, [])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    setBusy(true)
    try {
      const next = await saveSDNSettings({
        enabled,
        zone_name: zoneName,
        zone_type: 'simple',
        supernet,
        subnet_size: subnetSize,
        dns_server: dnsServer,
      })
      setView(next)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}
    >
      <div style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Per-user VNet isolation
      </div>
      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Each Nimbus user gets a dedicated Proxmox SDN VNet — VMs can talk
        within a user's own subnets but can't reach other users' VMs or the
        cluster's main LAN. Outbound internet works via NAT (simple zone).
        Bootstrap (Gopher tunnel, GPU env) runs through the qemu-guest-agent,
        so isolated VMs provision identically to flat-LAN ones.
      </p>

      {view && <ZoneStatusChip view={view} />}
      {enabled && <ReachabilityCallout tunnelEnabled={tunnelInfo?.enabled ?? false} />}

      <form
        onSubmit={handleSave}
        style={{ display: 'flex', flexDirection: 'column', gap: 12 }}
      >
        <label style={{ display: 'flex', alignItems: 'center', gap: 10, fontSize: 13 }}>
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            disabled={busy}
          />
          Isolate users on per-user Proxmox SDN VNets
        </label>

        <div className="n-field">
          <label className="n-label" htmlFor="sdn-zone">Zone name</label>
          <input
            id="sdn-zone"
            className="n-input"
            type="text"
            value={zoneName}
            onChange={(e) => setZoneName(e.target.value)}
            placeholder="nimbus"
            disabled={busy}
            maxLength={8}
          />
        </div>

        <SDNAddressSpace
          pool={supernet}
          onPoolChange={setSupernet}
          slice={subnetSize}
          onSliceChange={setSubnetSize}
          disabled={busy}
        />

        <div className="n-field">
          <label className="n-label" htmlFor="sdn-dns">
            DNS server (optional, applied to each subnet)
          </label>
          <input
            id="sdn-dns"
            className="n-input"
            type="text"
            value={dnsServer}
            onChange={(e) => setDNSServer(e.target.value)}
            placeholder="1.1.1.1"
            disabled={busy}
          />
        </div>

        {error && <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>}
        {saved && <p style={{ margin: 0, fontSize: 13, color: 'var(--good)' }}>Saved.</p>}

        <div>
          <button
            type="submit"
            className="n-btn n-btn-primary"
            disabled={busy}
            style={{ minWidth: 120 }}
          >
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </div>
  )
}

// SDNAddressSpace renders the supernet + slice size as a single
// composite control instead of two unrelated CIDR inputs. Surfaces
// the math live so admins see the tradeoff between slice size and
// max-user count without doing it in their head.
//
// The supernet is the IP address space all per-user subnets carve
// from. The "slice" is the prefix length of each per-user subnet
// inside that space. Together they answer "how many users can land
// in this pool, and how many IPs does each one get?"
function SDNAddressSpace({
  pool,
  onPoolChange,
  slice,
  onSliceChange,
  disabled,
}: {
  pool: string
  onPoolChange: (v: string) => void
  slice: number
  onSliceChange: (v: number) => void
  disabled: boolean
}) {
  // Parse the supernet's prefix to compute the live preview. Falls
  // through to a passive hint when the input doesn't parse yet (user
  // still typing) instead of yelling at them.
  const poolPrefix = parsePoolPrefix(pool)
  const preview =
    poolPrefix !== null && slice >= poolPrefix
      ? describeCarving(poolPrefix, slice)
      : null

  // Split the combined CIDR (e.g. "10.42.0.0/16") into the two
  // input boxes — network on the left, prefix on the right —
  // mirroring the gateway-IP control above. The parent state keeps
  // the combined string so the save payload doesn't need a second
  // assembly step.
  const split = splitCIDR(pool)
  const network = split.network
  const networkPrefix = split.prefix
  const setNetwork = (v: string) => onPoolChange(joinCIDR(v, networkPrefix))
  const setNetworkPrefix = (v: number) => onPoolChange(joinCIDR(network, v))

  return (
    <div className="n-field">
      <label className="n-label" htmlFor="sdn-supernet-network">
        IP address space for user subnets
      </label>
      <div style={{ display: 'flex', alignItems: 'stretch', gap: 6 }}>
        <input
          id="sdn-supernet-network"
          className="n-input"
          type="text"
          placeholder="10.42.0.0"
          value={network}
          onChange={(e) => setNetwork(e.target.value)}
          disabled={disabled}
          style={{ flex: 1 }}
        />
        <span
          aria-hidden="true"
          style={{
            display: 'flex',
            alignItems: 'center',
            padding: '0 6px',
            color: 'var(--ink-mute)',
            fontSize: 16,
            fontFamily: 'var(--font-mono, monospace)',
          }}
        >
          /
        </span>
        <input
          id="sdn-supernet-prefix"
          className="n-input"
          type="number"
          min={1}
          max={32}
          placeholder="16"
          value={networkPrefix === 0 ? '' : networkPrefix}
          onChange={(e) => setNetworkPrefix(Number(e.target.value))}
          aria-label="Address-space prefix length"
          disabled={disabled}
          style={{ width: 88, textAlign: 'center' }}
        />
      </div>
      <p style={{ margin: '6px 0 0', fontSize: 11, color: 'var(--ink-mute)', lineHeight: 1.5 }}>
        Private RFC1918 range Nimbus carves from. Pick something that
        doesn't overlap with your cluster LAN — VMs in this space are
        NAT'd to the cluster's upstream gateway.
      </p>

      <div style={{ marginTop: 14, display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 12, color: 'var(--ink-2)' }}>↳ Each user's subnet:</span>
        <select
          value={slice}
          onChange={(e) => onSliceChange(parseInt(e.target.value, 10) || 24)}
          disabled={disabled}
          className="n-input"
          style={{ width: 'auto', minWidth: 240 }}
        >
          {SLICE_CHOICES.map((s) => {
            const meta = describeSlice(s)
            return (
              <option key={s} value={s}>
                /{s} — {meta.hosts} usable IPs each{s === 24 ? ' (default)' : ''}
              </option>
            )
          })}
        </select>
      </div>
      {preview && (
        <p style={{ margin: '8px 0 0', fontSize: 11, color: 'var(--ink-mute)' }}>
          Yields <strong>{preview.maxSubnets}</strong> subnets ·{' '}
          <strong>{preview.hostsPerSubnet}</strong> usable IPs per subnet.
        </p>
      )}
    </div>
  )
}

// splitCIDR pulls a combined "10.42.0.0/16" into ("10.42.0.0", 16).
// Returns prefix=0 when the slash is missing or malformed so the
// number input renders empty rather than NaN. Parent state is the
// source of truth — this is purely view-time decomposition.
function splitCIDR(cidr: string): { network: string; prefix: number } {
  const idx = cidr.indexOf('/')
  if (idx < 0) {
    return { network: cidr, prefix: 0 }
  }
  const n = parseInt(cidr.slice(idx + 1), 10)
  return { network: cidr.slice(0, idx), prefix: Number.isFinite(n) ? n : 0 }
}

// joinCIDR is the inverse of splitCIDR — combines the two inputs
// back into the API-shaped string. Empty network OR zero prefix
// means "user is mid-edit"; keep whichever component is set so the
// other input doesn't drop on the next render.
function joinCIDR(network: string, prefix: number): string {
  if (prefix === 0) return network
  return `${network}/${prefix}`
}

// Slice choices we surface in the dropdown. /24 is the default — 254
// usable IPs is plenty for a typical user. Smaller (>= /25) is for
// dense deployments; larger (<= /23) is rare and the supernet starts
// constraining max users. Capped at /28 below — anything tighter
// (/29..-/30) is unusable because the gateway + a few reserves eat the
// whole space.
const SLICE_CHOICES: number[] = [22, 23, 24, 25, 26, 27, 28]

function describeSlice(prefix: number): { hosts: number } {
  // Total addresses in the slice = 2^(32 - prefix). Subtract 2 for
  // network + broadcast plus our own pool offsets (gateway + reserves)
  // — see the carve math in vnetmgr/subnets.go (10..-.<broadcast-5>).
  const total = 1 << (32 - prefix)
  return { hosts: Math.max(0, total - 16) }
}

function parsePoolPrefix(cidr: string): number | null {
  const match = /\/(\d{1,2})$/.exec(cidr.trim())
  if (!match) return null
  const n = parseInt(match[1], 10)
  if (!Number.isFinite(n) || n < 1 || n > 32) return null
  return n
}

function describeCarving(poolPrefix: number, slicePrefix: number): { maxSubnets: number; hostsPerSubnet: number } {
  const maxSubnets = 1 << (slicePrefix - poolPrefix)
  const hostsPerSubnet = describeSlice(slicePrefix).hosts
  return { maxSubnets, hostsPerSubnet }
}

function ZoneStatusChip({ view }: { view: SDNSettingsView }) {
  const meta = (() => {
    switch (view.zone_status) {
      case 'active':
        return {
          label: 'Zone active',
          color: 'var(--good)',
          bg: 'rgba(48,128,72,0.08)',
          border: 'rgba(48,128,72,0.25)',
        }
      case 'pending':
        return {
          label: 'Zone pending — will apply on next save',
          color: 'var(--warn)',
          bg: 'rgba(184,101,15,0.08)',
          border: 'rgba(184,101,15,0.25)',
        }
      case 'missing-pkg':
        return {
          label:
            'Proxmox SDN package not installed — apt install libpve-network-perl on every node',
          color: 'var(--err)',
          bg: 'rgba(184,58,58,0.08)',
          border: 'rgba(184,58,58,0.25)',
        }
      case 'unconfigured':
        return {
          label: 'Enabled but zone name unset',
          color: 'var(--warn)',
          bg: 'rgba(184,101,15,0.08)',
          border: 'rgba(184,101,15,0.25)',
        }
      case 'error':
        return {
          label: `Proxmox error: ${view.proxmox_error || 'unknown'}`,
          color: 'var(--err)',
          bg: 'rgba(184,58,58,0.08)',
          border: 'rgba(184,58,58,0.25)',
        }
      case 'disabled':
      default:
        return {
          label: 'Disabled — VMs will continue using vmbr0',
          color: 'var(--ink-mute)',
          bg: 'rgba(20,18,28,0.04)',
          border: 'var(--line)',
        }
    }
  })()
  return (
    <div
      style={{
        padding: '10px 14px',
        background: meta.bg,
        border: `1px solid ${meta.border}`,
        borderRadius: 8,
        fontSize: 12,
        color: meta.color,
        display: 'flex',
        justifyContent: 'space-between',
        gap: 12,
      }}
    >
      <span>{meta.label}</span>
      {view.zone_status === 'active' && view.vnet_count > 0 && (
        <span>
          {view.vnet_count} VNet{view.vnet_count === 1 ? '' : 's'}
        </span>
      )}
    </div>
  )
}

// ReachabilityCallout explains how users actually connect to VMs once
// per-user isolation is enabled. The IP shown on the result page isn't
// reachable from outside the subnet — that's the whole point of
// isolation — so the operator needs to wire up at least one user-facing
// connect path. Rendered only when the toggle is on; admins thinking
// about flipping it see the consequences before they save.
function ReachabilityCallout({ tunnelEnabled }: { tunnelEnabled: boolean }) {
  const Row = ({
    state,
    title,
    body,
  }: {
    state: 'ok' | 'pending' | 'manual'
    title: React.ReactNode
    body: React.ReactNode
  }) => {
    const dot =
      state === 'ok'
        ? { background: 'var(--good)', label: 'Configured' }
        : state === 'manual'
          ? { background: 'var(--ink-mute)', label: 'Manual' }
          : { background: 'var(--warn)', label: 'Not configured' }
    return (
      <li style={{ display: 'flex', gap: 10, alignItems: 'flex-start' }}>
        <span
          style={{
            marginTop: 6,
            width: 8,
            height: 8,
            borderRadius: '50%',
            flexShrink: 0,
            background: dot.background,
          }}
          aria-label={dot.label}
        />
        <div>
          <div style={{ fontSize: 13, fontWeight: 500, color: 'var(--ink)' }}>{title}</div>
          <div style={{ fontSize: 12, color: 'var(--ink-body)', lineHeight: 1.5 }}>{body}</div>
        </div>
      </li>
    )
  }
  return (
    <div
      style={{
        padding: '12px 14px',
        background: 'rgba(20,18,28,0.03)',
        border: '1px solid var(--line)',
        borderRadius: 8,
        fontSize: 12,
        display: 'flex',
        flexDirection: 'column',
        gap: 10,
      }}
    >
      <div style={{ fontSize: 12, fontWeight: 600, color: 'var(--ink)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
        How users reach isolated VMs
      </div>
      <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 10 }}>
        <Row
          state={tunnelEnabled ? 'ok' : 'pending'}
          title="Gopher reverse tunnel (recommended)"
          body={
            tunnelEnabled ? (
              <>
                Configured. Users tick <em>Expose SSH publicly</em> at provision time and Nimbus
                bootstraps a public SSH endpoint via the gateway.
              </>
            ) : (
              <>
                Not configured — wire it up on{' '}
                <Link to="/infrastructure/gopher" style={{ color: 'var(--ink)', textDecoration: 'underline' }}>
                  Infrastructure → Gopher tunnels
                </Link>
                . Without this, isolated VMs are only reachable via the Proxmox console or from another
                VM in the same subnet.
              </>
            )
          }
        />
        <Row
          state="manual"
          title="Proxmox noVNC console"
          body={
            <>
              Always available from the Proxmox web UI as <em>Console</em> on the VM. Cloud-init
              templates are SSH-key only by default, so the login prompt is currently a dead end —
              browser-SSH (next phase) replaces this.
            </>
          }
        />
        <Row
          state="pending"
          title="Browser SSH (coming soon)"
          body="Server-side SSH via the PVE host, streamed to xterm.js — works on isolated subnets without Gopher or external services. Tracked as the follow-up to this rollout."
        />
      </ul>
    </div>
  )
}
