import { useEffect, useState } from 'react'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import {
  getAccessCode,
  getAuthorizedGitHubOrgs,
  getAuthorizedGoogleDomains,
  getOAuthSettings,
  regenerateAccessCode,
  saveAuthorizedGitHubOrgs,
  saveAuthorizedGoogleDomains,
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

function SetupNotes({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <details
      style={{
        background: 'rgba(20,18,28,0.03)',
        border: '1px solid var(--line)',
        borderRadius: 10,
        padding: '10px 14px',
        fontSize: 13,
        color: 'var(--ink-body)',
        lineHeight: 1.55,
      }}
    >
      <summary
        style={{
          cursor: 'pointer',
          fontWeight: 500,
          color: 'var(--ink)',
          listStyle: 'none',
          display: 'flex',
          alignItems: 'center',
          gap: 6,
          userSelect: 'none',
        }}
      >
        <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>▸</span>
        {title}
      </summary>
      <div style={{ marginTop: 10, paddingLeft: 4 }}>{children}</div>
    </details>
  )
}

function GoogleDomainsPanel() {
  const [domains, setDomains] = useState<string[]>([])
  const [draft, setDraft] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    getAuthorizedGoogleDomains()
      .then((res) => setDomains(res.domains))
      .catch(() => setError('Failed to load authorized domains'))
      .finally(() => setLoading(false))
  }, [])

  const persist = async (next: string[]) => {
    setError(null)
    try {
      const res = await saveAuthorizedGoogleDomains(next)
      setDomains(res.domains)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    }
  }

  const addDomain = async () => {
    const cleaned = draft.trim().toLowerCase().replace(/^@/, '')
    if (!cleaned) return
    if (!/^[a-z0-9.-]+\.[a-z]{2,}$/.test(cleaned)) {
      setError('Enter a valid domain (e.g. example.com)')
      return
    }
    if (domains.includes(cleaned)) {
      setDraft('')
      return
    }
    setDraft('')
    await persist([...domains, cleaned])
  }

  const removeDomain = async (domain: string) => {
    await persist(domains.filter((d) => d !== domain))
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ color: 'var(--ink)', opacity: 0.7 }}>
            <GoogleIcon size={18} />
          </span>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
            Authorized Google domains
          </span>
        </div>
        <span
          className="n-pill"
          style={{
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
        >
          {domains.length} domain{domains.length === 1 ? '' : 's'}
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Google OAuth sign-ins from any of these domains skip the access code
        entirely. New Google accounts whose email domain is <em>not</em> on
        this list are blocked at sign-up. Leaving the list empty disables the
        bypass — every Google sign-in still requires the access code.
      </p>

      <SetupNotes title="Setup notes">
        <ul style={{ margin: '4px 0 0', paddingLeft: 18, display: 'flex', flexDirection: 'column', gap: 6 }}>
          <li>
            Enter a bare domain — e.g. <code className="n-mono">acm.ucla.edu</code>, <code className="n-mono">example.com</code>.
            A leading <code className="n-mono">@</code> is stripped automatically.
          </li>
          <li>
            The bypass is dynamic: adding or removing a domain takes effect
            on the user's very next request. A user already in the console
            on a removed domain is sent back to the verify form.
          </li>
          <li>
            For Google Workspace, this checks the email domain returned by
            Google's <code className="n-mono">/userinfo</code> endpoint —
            personal <code className="n-mono">@gmail.com</code> accounts and
            domain-aliased addresses are matched on the <em>literal</em>
            domain Google returns.
          </li>
        </ul>
      </SetupNotes>

      {loading ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      ) : (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          {domains.length === 0 && (
            <span style={{ fontSize: 13, color: 'var(--ink-mute)', fontStyle: 'italic' }}>
              No domains authorized.
            </span>
          )}
          {domains.map((d) => (
            <span
              key={d}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 8,
                padding: '6px 6px 6px 12px',
                borderRadius: 999,
                background: 'rgba(248,175,130,0.12)',
                border: '1px solid rgba(248,175,130,0.4)',
                fontSize: 13,
                color: '#9a5c2e',
                fontFamily: 'Geist Mono, monospace',
              }}
            >
              {d}
              <button
                type="button"
                aria-label={`Remove ${d}`}
                onClick={() => removeDomain(d)}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  width: 20,
                  height: 20,
                  borderRadius: '50%',
                  border: 'none',
                  background: 'rgba(154,92,46,0.1)',
                  color: '#9a5c2e',
                  cursor: 'pointer',
                  fontSize: 14,
                  lineHeight: 1,
                  padding: 0,
                }}
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}

      <form
        onSubmit={(e) => {
          e.preventDefault()
          void addDomain()
        }}
        style={{ display: 'flex', gap: 8 }}
      >
        <input
          className="n-input"
          type="text"
          placeholder="example.com"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          style={{ flex: 1 }}
        />
        <button type="submit" className="n-btn n-btn-primary" disabled={!draft.trim()}>
          Add
        </button>
      </form>

      {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
      {saved && !error && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
    </div>
  )
}

function GitHubOrgsPanel() {
  const [orgs, setOrgs] = useState<string[]>([])
  const [draft, setDraft] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    getAuthorizedGitHubOrgs()
      .then((res) => setOrgs(res.orgs))
      .catch(() => setError('Failed to load authorized orgs'))
      .finally(() => setLoading(false))
  }, [])

  const persist = async (next: string[]) => {
    setError(null)
    try {
      const res = await saveAuthorizedGitHubOrgs(next)
      setOrgs(res.orgs)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    }
  }

  const addOrg = async () => {
    const cleaned = draft.trim().toLowerCase().replace(/^@/, '')
    if (!cleaned) return
    // GitHub logins: alphanumeric + single hyphens, can't start/end with hyphen.
    if (!/^[a-z0-9](?:[a-z0-9]|-(?=[a-z0-9])){0,38}$/.test(cleaned)) {
      setError('Enter a valid GitHub org login (e.g. acm-ucla)')
      return
    }
    if (orgs.includes(cleaned)) {
      setDraft('')
      return
    }
    setDraft('')
    await persist([...orgs, cleaned])
  }

  const removeOrg = async (org: string) => {
    await persist(orgs.filter((o) => o !== org))
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ color: 'var(--ink)', opacity: 0.7 }}>
            <GithubIcon size={18} />
          </span>
          <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
            Authorized GitHub organizations
          </span>
        </div>
        <span
          className="n-pill"
          style={{
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
        >
          {orgs.length} org{orgs.length === 1 ? '' : 's'}
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        GitHub OAuth sign-ins from members of any of these organizations skip
        the access code entirely. Sign-ins from accounts <em>not</em> in any
        listed org are blocked at the GitHub callback. Leaving the list empty
        disables the bypass — every GitHub sign-in still requires the access
        code.
      </p>

      <SetupNotes title="Setup notes">
        <ul style={{ margin: '4px 0 0', paddingLeft: 18, display: 'flex', flexDirection: 'column', gap: 6 }}>
          <li>
            Enter the org's GitHub login — e.g. <code className="n-mono">acm-ucla</code>,
            not the display name. Find it in the org URL:{' '}
            <code className="n-mono">github.com/&lt;login&gt;</code>.
          </li>
          <li>
            Org membership is captured at each GitHub login, including
            private memberships (we request the <code className="n-mono">read:org</code> scope).
          </li>
          <li>
            <strong>Org SSO orgs</strong> (most enterprise GitHub orgs)
            require the Nimbus OAuth app to be authorized at the org level.
            The org owner must approve the app on the org's
            "Third-party access" page — until then, member queries from the
            Nimbus app will return empty for that org.
          </li>
          <li>
            User snapshots refresh on every GitHub login, so a user newly
            added to an authorized org just needs to sign in once via GitHub
            to gain bypass.
          </li>
        </ul>
      </SetupNotes>

      {loading ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      ) : (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          {orgs.length === 0 && (
            <span style={{ fontSize: 13, color: 'var(--ink-mute)', fontStyle: 'italic' }}>
              No organizations authorized.
            </span>
          )}
          {orgs.map((o) => (
            <span
              key={o}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 8,
                padding: '6px 6px 6px 12px',
                borderRadius: 999,
                background: 'rgba(27,23,38,0.06)',
                border: '1px solid var(--line)',
                fontSize: 13,
                color: 'var(--ink)',
                fontFamily: 'Geist Mono, monospace',
              }}
            >
              {o}
              <button
                type="button"
                aria-label={`Remove ${o}`}
                onClick={() => removeOrg(o)}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  width: 20,
                  height: 20,
                  borderRadius: '50%',
                  border: 'none',
                  background: 'rgba(20,18,28,0.08)',
                  color: 'var(--ink)',
                  cursor: 'pointer',
                  fontSize: 14,
                  lineHeight: 1,
                  padding: 0,
                }}
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}

      <form
        onSubmit={(e) => {
          e.preventDefault()
          void addOrg()
        }}
        style={{ display: 'flex', gap: 8 }}
      >
        <input
          className="n-input"
          type="text"
          placeholder="acm-ucla"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          style={{ flex: 1 }}
        />
        <button type="submit" className="n-btn n-btn-primary" disabled={!draft.trim()}>
          Add
        </button>
      </form>

      {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
      {saved && !error && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
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
              <GitHubOrgsPanel />
              <ProviderPanel
                name="Google"
                icon={<GoogleIcon size={20} />}
                clientId={settings.google_client_id}
                configured={settings.google_configured}
                instructionsUrl="https://console.cloud.google.com/apis/credentials"
                instructionsLabel="Google Cloud Console"
                onSave={handleSaveGoogle}
              />
              <GoogleDomainsPanel />
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
