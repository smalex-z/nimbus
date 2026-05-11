import { useEffect, useState } from 'react'
import { getGopherSettings, saveGopherSettings } from '@/api/client'
import type { GopherSettingsView } from '@/api/client'
import SelfBootstrapModal from '@/components/ui/SelfBootstrapModal'

// DNS-label rule the backend enforces — surfaced client-side so the user
// sees the error before the round-trip. 1-63 chars, a-z/0-9/hyphen, no
// leading or trailing hyphen.
const DNS_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/

// GopherPanel renders the Gopher credentials + cloud-tunnel state UI.
// Shared between the Infrastructure → Gopher tunnels page and the
// install wizard's Network step. Both surfaces hit the same backend
// (/api/settings/gopher/*) — the install wizard now mounts those
// endpoints in setup mode too so this component works pre-config.
//
// Self-contained: owns its own fetch + save + modal pop. The only
// integration point with callers is the optional onTunnelActive
// callback fired when the cloud tunnel reaches StateActive, used by
// the install wizard to enable its 'Next' button.
export default function GopherPanel({ onTunnelActive }: { onTunnelActive?: () => void } = {}) {
  const [settings, setSettings] = useState<GopherSettingsView | null>(null)
  const [apiURL, setAPIURL] = useState('')
  const [apiKey, setAPIKey] = useState('')
  const [subdomain, setSubdomain] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [bootstrapOpen, setBootstrapOpen] = useState(false)

  useEffect(() => {
    getGopherSettings()
      .then((s) => {
        setSettings(s)
        setAPIURL(s.api_url)
        setSubdomain(s.cloud_subdomain)
        if (s.tunnel_active && onTunnelActive) {
          onTunnelActive()
        }
      })
      .catch(() => setError('Failed to load Gopher settings'))
    // onTunnelActive is captured at mount; we don't want to re-fire on
    // every re-render — only when the initial fetch reveals the
    // tunnel was already up from a prior save.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const trimmedSubdomain = subdomain.trim().toLowerCase()
  const subdomainChanged =
    settings != null && trimmedSubdomain !== '' && trimmedSubdomain !== settings.cloud_subdomain
  const subdomainInvalid = trimmedSubdomain !== '' && !DNS_LABEL.test(trimmedSubdomain)

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)

    if (subdomainInvalid) {
      setError('Cloud subdomain must be a DNS label: a-z, 0-9, or hyphen (no leading/trailing hyphen).')
      return
    }

    if (subdomainChanged && settings) {
      const ok = window.confirm(
        `Change cloud subdomain from "${settings.cloud_subdomain}" to "${trimmedSubdomain}"?\n\n` +
          `Nimbus will tear down the existing tunnel and rebuild it under the new hostname. ` +
          `Any OAuth provider that has the old redirect URI registered (Google Cloud Console, ` +
          `GitHub OAuth app) will stop accepting sign-ins until you update the redirect URI to ` +
          `point at the new hostname.`,
      )
      if (!ok) return
    }

    try {
      setSaving(true)
      const next = await saveGopherSettings({
        api_url: apiURL,
        api_key: apiKey,
        cloud_subdomain: trimmedSubdomain,
      })
      setSettings(next)
      setAPIURL(next.api_url)
      setSubdomain(next.cloud_subdomain)
      setAPIKey('')
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
      if (next.credentials_saved) setBootstrapOpen(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  // After the modal closes, re-fetch state to pick up the bootstrap
  // outcome (cloud_tunnel_url, tunnel_active). The wizard callback
  // fires from here when tunnel_active becomes true so the operator
  // can advance.
  const refreshAfterBootstrap = async () => {
    setBootstrapOpen(false)
    try {
      const s = await getGopherSettings()
      setSettings(s)
      setAPIURL(s.api_url)
      setSubdomain(s.cloud_subdomain)
      if (s.tunnel_active && onTunnelActive) {
        onTunnelActive()
      }
    } catch {
      // Non-fatal — keep current state.
    }
  }

  const credentialsSaved = settings?.credentials_saved ?? false
  const tunnelActive = settings?.tunnel_active ?? false

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Gopher tunnels
        </span>
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          <StatusPill ok={credentialsSaved} okText="credentials saved" muteText="no credentials" />
          <StatusPill ok={tunnelActive} okText="cloud tunnel active" muteText="cloud tunnel inactive" />
        </div>
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
            {credentialsSaved && (
              <span style={{ marginLeft: 6, fontSize: 11, color: 'var(--ink-mute)', fontWeight: 400 }}>
                (leave blank to keep existing)
              </span>
            )}
          </label>
          <input
            id="gopher-api-key"
            className="n-input"
            type="password"
            placeholder={credentialsSaved ? '••••••••' : 'Paste your Gopher API key'}
            value={apiKey}
            onChange={(e) => setAPIKey(e.target.value)}
          />
        </div>
        <div className="n-field">
          <label className="n-label" htmlFor="gopher-cloud-subdomain">
            Cloud subdomain
          </label>
          <input
            id="gopher-cloud-subdomain"
            className="n-input"
            type="text"
            placeholder="cloud"
            value={subdomain}
            onChange={(e) => setSubdomain(e.target.value)}
            style={{ fontFamily: 'Geist Mono, monospace' }}
          />
          <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.5 }}>
            Leftmost label of the public hostname Nimbus is exposed at — defaults to{' '}
            <code style={{ fontFamily: 'Geist Mono, monospace' }}>cloud</code>. Override when two
            Nimbus instances share one Gopher domain (e.g.{' '}
            <code style={{ fontFamily: 'Geist Mono, monospace' }}>cloud-dev</code> for the dev
            instance).
          </p>
          {subdomainInvalid && (
            <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--err)', lineHeight: 1.5 }}>
              DNS label only: a-z, 0-9, or hyphen (no leading/trailing hyphen, max 63 chars).
            </p>
          )}
          {subdomainChanged && !subdomainInvalid && (
            <p style={{ margin: '6px 0 0', fontSize: 12, color: '#9a5c2e', lineHeight: 1.5 }}>
              Changing the subdomain rebuilds the public tunnel. OAuth providers that have the old
              redirect URI registered will stop accepting sign-ins until you update them on the IdP
              side.
            </p>
          )}
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
      {bootstrapOpen && <SelfBootstrapModal onClose={refreshAfterBootstrap} />}
    </div>
  )
}

function StatusPill({ ok, okText, muteText }: { ok: boolean; okText: string; muteText: string }) {
  if (ok) {
    return (
      <span className="n-pill n-pill-ok">
        <span className="n-pill-dot" />
        {okText}
      </span>
    )
  }
  return (
    <span
      className="n-pill"
      style={{
        color: 'var(--ink-mute)',
        background: 'rgba(20,18,28,0.04)',
        border: '1px solid var(--line)',
      }}
    >
      {muteText}
    </span>
  )
}
