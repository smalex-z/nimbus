import { useEffect, useState } from 'react'
import {
  forceGatewayUpdate,
  getNetworkSettings,
  renumberAllVMs,
  saveNetworkSettings,
} from '@/api/client'
import type { NetworkOpReport, NetworkSettingsView } from '@/api/client'

type DangerKind = 'renumber' | 'force-gateway'

function NetworkPanel() {
  const [settings, setSettings] = useState<NetworkSettingsView | null>(null)
  const [poolStart, setPoolStart] = useState('')
  const [poolEnd, setPoolEnd] = useState('')
  const [gateway, setGateway] = useState('')
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
      })
      setSettings(next)
      setPoolStart(next.ip_pool_start)
      setPoolEnd(next.ip_pool_end)
      setGateway(next.gateway_ip)
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
      gateway !== settings.gateway_ip)

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
          <label className="n-label" htmlFor="net-gateway">Gateway IP</label>
          <input
            id="net-gateway"
            className="n-input"
            type="text"
            placeholder="192.168.0.1"
            value={gateway}
            onChange={(e) => setGateway(e.target.value)}
          />
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
            className="n-btn"
            disabled={dirty}
            onClick={() => {
              setReport(null)
              setReportKind(null)
              setDanger('force-gateway')
            }}
            title={dirty ? 'Save your changes first' : ''}
          >
            Force gateway on every VM…
          </button>
          <button
            type="button"
            className="n-btn"
            disabled={dirty}
            onClick={() => {
              setReport(null)
              setReportKind(null)
              setDanger('renumber')
            }}
            title={dirty ? 'Save your changes first' : ''}
          >
            Renumber every VM into new pool…
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
            {reportKind === 'renumber' ? 'Renumber complete' : 'Gateway update complete'}
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
    kind === 'renumber' ? 'RENUMBER ALL VMS' : 'CHANGE GATEWAY ON ALL VMS'
  const [typed, setTyped] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const title =
    kind === 'renumber'
      ? 'Renumber every nimbus-managed VM'
      : 'Force the new gateway on every VM'

  const description =
    kind === 'renumber'
      ? `Every nimbus-managed VM will be assigned a fresh IP from ${
          settings?.ip_pool_start ?? '?'
        } – ${settings?.ip_pool_end ?? '?'} and rebooted. The new pool must have at least as many free addresses as you have VMs. Any open SSH sessions will drop. This cannot be cleanly undone — the old IPs are released back to the pool.`
      : `Every nimbus-managed VM will be reconfigured to use ${
          settings?.gateway_ip ?? '?'
        } as its default gateway and rebooted. If the new gateway is not yet reachable on your network, every VM will lose connectivity until the network is fixed. Any open SSH sessions will drop.`

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
            {running ? 'Working…' : kind === 'renumber' ? 'Renumber all VMs' : 'Force gateway'}
          </button>
        </div>
      </div>
    </div>
  )
}

export default function Network() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Network
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          The IP pool nimbus draws from when provisioning, and the gateway
          handed to each VM via cloud-init.
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          <NetworkPanel />
        </div>
      </div>
    </div>
  )
}
