import { useEffect, useState } from 'react'
import {
  getGPUSettings,
  mintGPUPairingToken,
  saveGPUSettings,
  unpairGX10,
} from '@/api/client'
import type { GPUSettingsView } from '@/types'

// GPUHost — pairing-first GX10 onboarding.
//
// The Add GX10 button mints a 5-min pairing token and hands back a
// pre-baked `curl ... | sudo bash` line. The operator pastes that on the
// GX10; the install script self-registers, gets a worker token, picks up
// inference base URL from the GX10's reported IP, and brings up both
// systemd units.
//
// Post-pairing the page shows what registered (hostname + base URL + model)
// and lets admins edit base URL / model in case the GX10 has multiple NICs
// or they want to swap the default vLLM model later. Re-pair by clicking
// Add GX10 again — the new pairing wipes the prior worker token.
export default function GPUHost() {
  const [settings, setSettings] = useState<GPUSettingsView | null>(null)
  const [baseURL, setBaseURL] = useState('')
  const [model, setModel] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pairingCmd, setPairingCmd] = useState<string | null>(null)
  const [pairingExpiresIn, setPairingExpiresIn] = useState(0)
  const [pairingCopied, setPairingCopied] = useState(false)
  const [pairing, setPairing] = useState(false)
  // Unpair flow uses a two-click confirm: first click flips this, second
  // click within 4s actually fires. Avoids accidental wipes.
  const [unpairConfirming, setUnpairConfirming] = useState(false)
  const [unpairing, setUnpairing] = useState(false)
  const [unpairResult, setUnpairResult] = useState<{ cancelledJobs: number; cleanupCmd: string } | null>(null)
  const [cleanupCopied, setCleanupCopied] = useState(false)

  useEffect(() => {
    getGPUSettings()
      .then((s) => {
        setSettings(s)
        setBaseURL(s.base_url)
        setModel(s.inference_model)
      })
      .catch(() => setError('Failed to load GPU settings'))
  }, [])

  // Tick down the pairing token's TTL so the operator can see how long
  // they have left to paste the curl command on the GX10.
  useEffect(() => {
    if (pairingExpiresIn <= 0) return
    const id = setInterval(() => setPairingExpiresIn((n) => Math.max(0, n - 1)), 1_000)
    return () => clearInterval(id)
  }, [pairingExpiresIn])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    try {
      setSaving(true)
      const next = await saveGPUSettings({
        enabled: settings?.enabled ?? true,
        base_url: baseURL,
        inference_model: model,
      })
      setSettings(next)
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const handleUnpair = async () => {
    if (!unpairConfirming) {
      setUnpairConfirming(true)
      // Auto-revert after 4s so a stray click doesn't leave the button in
      // confirm-mode forever.
      setTimeout(() => setUnpairConfirming(false), 4_000)
      return
    }
    setError(null)
    setUnpairing(true)
    try {
      const r = await unpairGX10()
      setUnpairResult({ cancelledJobs: r.cancelled_jobs, cleanupCmd: r.cleanup_cmd })
      // Refetch settings so the panel rerenders in the unpaired state.
      const fresh = await getGPUSettings()
      setSettings(fresh)
      setBaseURL(fresh.base_url)
      setModel(fresh.inference_model)
      // Wipe any stale pairing display.
      setPairingCmd(null)
      setPairingExpiresIn(0)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unpair failed')
    } finally {
      setUnpairing(false)
      setUnpairConfirming(false)
    }
  }

  const handleAddGX10 = async () => {
    setError(null)
    setPairing(true)
    try {
      const r = await mintGPUPairingToken()
      setPairingCmd(r.curl)
      setPairingExpiresIn(r.expires_in_seconds)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to mint pairing token')
    } finally {
      setPairing(false)
    }
  }

  const configured = settings?.configured ?? false

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          GX10 GPU plane
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Pair a GX10 (or any aarch64 NVIDIA host) with this Nimbus instance.
          Once paired, every VM gets <code className="n-mono">OPENAI_BASE_URL</code> +
          a <code className="n-mono">gx10</code> CLI helper, and the GPU jobs
          tab appears for everyone.
        </p>
      </div>

      <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
            Pairing
          </span>
          {configured ? (
            <span className="n-pill n-pill-ok">
              <span className="n-pill-dot" />
              paired{settings?.gx10_hostname ? ` · ${settings.gx10_hostname}` : ''}
            </span>
          ) : (
            <span
              className="n-pill"
              style={{
                color: 'var(--ink-mute)',
                background: 'rgba(20,18,28,0.04)',
                border: '1px solid var(--line)',
              }}
            >
              no GX10 paired
            </span>
          )}
        </div>

        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Click Add GX10 to mint a 5-minute pairing command. SSH into the GX10
          and paste it — the script registers itself, installs vLLM, and brings
          up the job worker. No tokens to copy by hand.
        </p>

        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <button
            type="button"
            className="n-btn n-btn-primary"
            onClick={handleAddGX10}
            disabled={pairing || unpairing}
          >
            {pairing ? 'Generating…' : configured ? 'Re-pair GX10' : 'Add GX10'}
          </button>
          {configured && (
            <button
              type="button"
              className="n-btn"
              onClick={handleUnpair}
              disabled={pairing || unpairing}
              style={unpairConfirming ? { borderColor: 'var(--err)', color: 'var(--err)' } : undefined}
              title="Wipes the worker token, cancels queued/running jobs, and disables the GPU plane. The GX10 itself keeps running its systemd units until you tear them down on the host."
            >
              {unpairing
                ? 'Unpairing…'
                : unpairConfirming
                  ? 'Click again to confirm'
                  : 'Unpair GX10'}
            </button>
          )}
          {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
        </div>

        {unpairResult && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginTop: 4 }}>
            <span style={{ fontSize: 13, color: 'var(--ink)' }}>
              Unpaired{unpairResult.cancelledJobs > 0 ? ` · ${unpairResult.cancelledJobs} job${unpairResult.cancelledJobs === 1 ? '' : 's'} cancelled` : ''}.
            </span>
            <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
              Run on the GX10 to stop and remove the systemd units:
            </span>
            <div style={{ display: 'flex', gap: 8, alignItems: 'stretch' }}>
              <code style={{
                flex: 1,
                padding: '8px 12px',
                background: 'rgba(20,18,28,0.05)',
                border: '1px solid var(--line)',
                borderRadius: 6,
                fontSize: 12,
                fontFamily: 'monospace',
                wordBreak: 'break-all',
              }}>{unpairResult.cleanupCmd}</code>
              <button
                type="button"
                className="n-btn"
                onClick={async () => {
                  try {
                    await navigator.clipboard.writeText(unpairResult.cleanupCmd)
                    setCleanupCopied(true)
                    setTimeout(() => setCleanupCopied(false), 1500)
                  } catch { /* noop */ }
                }}
              >
                {cleanupCopied ? 'Copied' : 'Copy'}
              </button>
            </div>
          </div>
        )}

        {pairingCmd && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
              Run on the GX10 ({pairingExpiresIn > 0 ? `expires in ${formatRemaining(pairingExpiresIn)}` : 'expired — click Add GX10 again'}):
            </span>
            <div style={{ display: 'flex', gap: 8, alignItems: 'stretch' }}>
              <code style={{
                flex: 1,
                padding: '8px 12px',
                background: 'rgba(20,18,28,0.05)',
                border: '1px solid var(--line)',
                borderRadius: 6,
                fontSize: 12,
                fontFamily: 'monospace',
                wordBreak: 'break-all',
                opacity: pairingExpiresIn > 0 ? 1 : 0.5,
              }}>{pairingCmd}</code>
              <button
                type="button"
                className="n-btn"
                disabled={pairingExpiresIn <= 0}
                onClick={async () => {
                  try {
                    await navigator.clipboard.writeText(pairingCmd)
                    setPairingCopied(true)
                    setTimeout(() => setPairingCopied(false), 1500)
                  } catch { /* noop */ }
                }}
              >
                {pairingCopied ? 'Copied' : 'Copy'}
              </button>
            </div>
          </div>
        )}
      </div>

      {configured && (
        <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
            Live settings
          </span>
          <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
            Auto-populated by the pairing handshake. Edit if the GX10 has
            multiple NICs (override base URL) or you want to swap the
            default vLLM model.
          </p>

          <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="n-field">
              <label className="n-label" htmlFor="gpu-base-url">Inference base URL</label>
              <input
                id="gpu-base-url"
                className="n-input"
                type="text"
                placeholder="http://gx10.lan:8000"
                value={baseURL}
                onChange={(e) => setBaseURL(e.target.value)}
              />
            </div>
            <div className="n-field">
              <label className="n-label" htmlFor="gpu-model">Default model</label>
              <input
                id="gpu-model"
                className="n-input"
                type="text"
                placeholder="microsoft/Phi-3-mini-4k-instruct"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
            </div>

            <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <button
                type="submit"
                className="n-btn n-btn-primary"
                disabled={saving}
                style={{ minWidth: 100 }}
              >
                {saving ? 'Saving…' : 'Save'}
              </button>
              {saved && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
            </div>
          </form>
        </div>
      )}
    </div>
  )
}

// formatRemaining renders 0–599 seconds as "Mm Ss" — enough resolution for
// the operator to see the pairing window winding down.
function formatRemaining(secs: number): string {
  const m = Math.floor(secs / 60)
  const s = secs % 60
  if (m > 0) return `${m}m ${s.toString().padStart(2, '0')}s`
  return `${s}s`
}
