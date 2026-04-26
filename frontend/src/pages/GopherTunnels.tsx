import { useEffect, useState } from 'react'
import { getGopherSettings, saveGopherSettings } from '@/api/client'
import type { GopherSettingsView } from '@/api/client'
import SelfBootstrapModal from '@/components/ui/SelfBootstrapModal'

function GopherPanel() {
  const [settings, setSettings] = useState<GopherSettingsView | null>(null)
  const [apiURL, setAPIURL] = useState('')
  const [apiKey, setAPIKey] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Pops the self-bootstrap modal after a successful save. Backend kicks
  // off the bootstrap automatically; the modal just polls + reports state.
  const [bootstrapOpen, setBootstrapOpen] = useState(false)

  useEffect(() => {
    getGopherSettings()
      .then((s) => {
        setSettings(s)
        setAPIURL(s.api_url)
      })
      .catch(() => setError('Failed to load Gopher settings'))
  }, [])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    try {
      setSaving(true)
      const next = await saveGopherSettings({ api_url: apiURL, api_key: apiKey })
      setSettings(next)
      setAPIURL(next.api_url)
      setAPIKey('')
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
      // SaveGopher kicks off self-bootstrap server-side when creds are
      // valid; pop the modal so the admin sees the phase indicator and
      // (on success) gets redirected to the cloud URL.
      if (next.configured) setBootstrapOpen(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const configured = settings?.configured ?? false

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Gopher tunnels
        </span>
        {configured ? (
          <span className="n-pill n-pill-ok">
            <span className="n-pill-dot" />
            configured
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
            not configured
          </span>
        )}
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Gopher is the reverse-tunnel gateway used to expose VMs at public
        hostnames. Provide the API URL + key and Nimbus can request tunnels at
        provision time. Leave both blank to disable tunneling.
      </p>

      <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="n-field">
          <label className="n-label" htmlFor="gopher-api-url">API URL</label>
          <input
            id="gopher-api-url"
            className="n-input"
            type="text"
            placeholder="https://gopher.example.com"
            value={apiURL}
            onChange={(e) => setAPIURL(e.target.value)}
          />
        </div>
        <div className="n-field">
          <label className="n-label" htmlFor="gopher-api-key">
            API key
            {configured && (
              <span style={{ marginLeft: 6, fontSize: 11, color: 'var(--ink-mute)', fontWeight: 400 }}>
                (leave blank to keep existing)
              </span>
            )}
          </label>
          <input
            id="gopher-api-key"
            className="n-input"
            type="password"
            placeholder={configured ? '••••••••' : 'Paste your Gopher API key'}
            value={apiKey}
            onChange={(e) => setAPIKey(e.target.value)}
          />
        </div>

        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}

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
      {bootstrapOpen && <SelfBootstrapModal onClose={() => setBootstrapOpen(false)} />}
    </div>
  )
}

export default function GopherTunnels() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Gopher tunnels
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Configure the reverse-tunnel gateway Nimbus uses to expose VMs at
          public hostnames.
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          <GopherPanel />
        </div>
      </div>
    </div>
  )
}
