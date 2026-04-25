import { useEffect, useState } from 'react'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import {
  getAccessCode,
  getOAuthSettings,
  regenerateAccessCode,
  saveOAuthSettings,
} from '@/api/client'
import type { AccessCodeView, OAuthSettingsView } from '@/api/client'

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

function EyeIcon({ open }: { open: boolean }) {
  if (open) {
    return (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7z" />
        <circle cx="12" cy="12" r="3" />
      </svg>
    )
  }
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17.94 17.94A10.94 10.94 0 0 1 12 19c-7 0-10-7-10-7a19.6 19.6 0 0 1 4.22-5.94" />
      <path d="M9.9 4.24A10.94 10.94 0 0 1 12 4c7 0 10 7 10 7a19.5 19.5 0 0 1-2.16 3.19" />
      <path d="M14.12 14.12A3 3 0 1 1 9.88 9.88" />
      <line x1="2" y1="2" x2="22" y2="22" />
    </svg>
  )
}

function CopyIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
    </svg>
  )
}

function RefreshIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="23 4 23 10 17 10" />
      <polyline points="1 20 1 14 7 14" />
      <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10" />
      <path d="M20.49 15a9 9 0 0 1-14.85 3.36L1 14" />
    </svg>
  )
}

function AccessCodePanel() {
  const [code, setCode] = useState<AccessCodeView | null>(null)
  const [revealed, setRevealed] = useState(false)
  const [copied, setCopied] = useState(false)
  const [regenerating, setRegenerating] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    getAccessCode()
      .then(setCode)
      .catch(() => setError('Failed to load access code'))
  }, [])

  const masked = code ? '•'.repeat(code.access_code.length) : '••••••••'
  const display = revealed && code ? code.access_code : masked

  const copy = async () => {
    if (!code) return
    try {
      await navigator.clipboard.writeText(code.access_code)
      setCopied(true)
      setTimeout(() => setCopied(false), 1800)
    } catch {
      setError('Copy failed — clipboard unavailable')
    }
  }

  const regenerate = async () => {
    if (!confirming) {
      setConfirming(true)
      setTimeout(() => setConfirming(false), 4000)
      return
    }
    setError(null)
    try {
      setRegenerating(true)
      const next = await regenerateAccessCode()
      setCode(next)
      setRevealed(true)
      setConfirming(false)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Regenerate failed')
    } finally {
      setRegenerating(false)
    }
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>Access code</span>
        </div>
        <span className="n-pill n-pill-ok">
          <span className="n-pill-dot" />
          active
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Non-admin users must enter this 8-digit code after signing in to access
        the console. Regenerating the code immediately invalidates every
        non-admin user&apos;s prior verification — they&apos;ll be sent back to
        the verify form on their next action.
      </p>

      <div
        style={{
          padding: '14px 16px',
          background: 'rgba(20,18,28,0.04)',
          border: '1px solid var(--line)',
          borderRadius: 10,
          fontFamily: 'Geist Mono, monospace',
          fontSize: 20,
          letterSpacing: revealed ? '0.16em' : '0.28em',
          color: 'var(--ink)',
          textAlign: 'center',
          wordBreak: 'break-all',
        }}
      >
        {display}
      </div>

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <button
          type="button"
          aria-label={revealed ? 'Hide code' : 'Show code'}
          onClick={() => setRevealed((v) => !v)}
          className="n-btn n-btn-secondary"
          style={{ flex: 1, minWidth: 0, padding: '8px 10px', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', gap: 6, fontSize: 13 }}
        >
          <EyeIcon open={revealed} />
          {revealed ? 'Hide' : 'Show'}
        </button>
        <button
          type="button"
          aria-label="Copy code"
          onClick={copy}
          className="n-btn n-btn-secondary"
          style={{ flex: 1, minWidth: 0, padding: '8px 10px', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', gap: 6, fontSize: 13 }}
        >
          <CopyIcon />
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>

      <button
        type="button"
        onClick={regenerate}
        disabled={regenerating}
        className="n-btn n-btn-secondary"
        style={{ display: 'inline-flex', alignItems: 'center', justifyContent: 'center', gap: 8, width: '100%' }}
      >
        <RefreshIcon />
        {regenerating ? 'Regenerating…' : confirming ? 'Click again to confirm' : 'Regenerate'}
      </button>

      {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
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
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Authentication
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Manage the access code and OAuth providers users can sign in with.
        </p>
      </div>

      {loadError && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{loadError}</p>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
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
        <div className="lg:col-span-1">
          <AccessCodePanel />
        </div>
      </div>
    </div>
  )
}
