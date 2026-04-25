import { useEffect, useState } from 'react'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import { getOAuthSettings, saveOAuthSettings } from '@/api/client'
import type { OAuthSettingsView } from '@/api/client'

interface ProviderPanelProps {
  name: string
  icon: React.ReactNode
  clientId: string
  configured: boolean
  instructionsUrl: string
  instructionsLabel: string
  onSave: (clientId: string, clientSecret: string) => Promise<unknown>
}

function ProviderPanel({
  name,
  icon,
  clientId: initialClientId,
  configured,
  instructionsUrl,
  instructionsLabel,
  onSave,
}: ProviderPanelProps) {
  const [clientId, setClientId] = useState(initialClientId)
  const [clientSecret, setClientSecret] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setClientId(initialClientId)
  }, [initialClientId])

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    try {
      setSaving(true)
      await onSave(clientId, clientSecret)
      setClientSecret('')
      setSaved(true)
      setTimeout(() => setSaved(false), 3000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 20 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ color: 'var(--ink)', opacity: 0.7 }}>{icon}</span>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>{name}</span>
        </div>
        {configured ? (
          <span className="n-pill n-pill-ok">
            <span className="n-pill-dot" />
            configured
          </span>
        ) : (
          <span className="n-pill" style={{ color: 'var(--ink-mute)', background: 'rgba(20,18,28,0.04)', border: '1px solid var(--line)' }}>
            not configured
          </span>
        )}
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Create an OAuth app at{' '}
        <a
          href={instructionsUrl}
          target="_blank"
          rel="noreferrer"
          className="n-link"
        >
          {instructionsLabel}
        </a>{' '}
        and enter the credentials below. Users will be able to sign in with {name} once configured.
      </p>

      <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="n-field">
          <label className="n-label" htmlFor={`${name}-client-id`}>
            Client ID
          </label>
          <input
            id={`${name}-client-id`}
            className="n-input"
            type="text"
            placeholder="Paste your client ID"
            value={clientId}
            onChange={(e) => setClientId(e.target.value)}
          />
        </div>

        <div className="n-field">
          <label className="n-label" htmlFor={`${name}-client-secret`}>
            Client Secret
            {configured && (
              <span style={{ marginLeft: 6, fontSize: 11, color: 'var(--ink-mute)', fontWeight: 400 }}>
                (leave blank to keep existing)
              </span>
            )}
          </label>
          <input
            id={`${name}-client-secret`}
            className="n-input"
            type="password"
            placeholder={configured ? '••••••••' : 'Paste your client secret'}
            value={clientSecret}
            onChange={(e) => setClientSecret(e.target.value)}
          />
        </div>

        {error && (
          <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
        )}

        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button
            className="n-btn n-btn-primary"
            type="submit"
            disabled={saving}
            style={{ minWidth: 100 }}
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
          {saved && (
            <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>
          )}
        </div>
      </form>
    </div>
  )
}

export default function Settings() {
  const [settings, setSettings] = useState<OAuthSettingsView | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    getOAuthSettings()
      .then(setSettings)
      .catch(() => setLoadError('Failed to load OAuth settings'))
  }, [])

  const handleSaveGitHub = async (clientId: string, clientSecret: string) => {
    const updated = await saveOAuthSettings({
      github_client_id: clientId,
      github_client_secret: clientSecret,
    })
    // Refresh settings after save
    const fresh = await getOAuthSettings()
    setSettings(fresh)
    return updated
  }

  const handleSaveGoogle = async (clientId: string, clientSecret: string) => {
    const updated = await saveOAuthSettings({
      google_client_id: clientId,
      google_client_secret: clientSecret,
    })
    const fresh = await getOAuthSettings()
    setSettings(fresh)
    return updated
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28, maxWidth: 700 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Settings
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Configure OAuth providers to let users sign in with third-party accounts.
        </p>
      </div>

      {loadError && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{loadError}</p>
      )}

      {settings && (
        <>
          <ProviderPanel
            name="GitHub"
            icon={<GithubIcon size={20} />}
            clientId={settings.github_client_id}
            configured={settings.github_configured}
            instructionsUrl="https://github.com/settings/applications/new"
            instructionsLabel="github.com/settings/applications/new"
            onSave={handleSaveGitHub}
          />
          <ProviderPanel
            name="Google"
            icon={<GoogleIcon size={20} />}
            clientId={settings.google_client_id}
            configured={settings.google_configured}
            instructionsUrl="https://console.cloud.google.com/apis/credentials"
            instructionsLabel="Google Cloud Console"
            onSave={handleSaveGoogle}
          />
        </>
      )}
    </div>
  )
}
