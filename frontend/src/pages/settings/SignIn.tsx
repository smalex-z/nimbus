import { useCallback, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import UsersTable from '@/components/UsersTable'
import {
  emailUnlinkedUsers,
  getAccessCode,
  getAuthorizedGitHubOrgs,
  getAuthorizedGoogleDomains,
  getOAuthSettings,
  getPasswordlessStatus,
  regenerateAccessCode,
  saveAuthorizedGitHubOrgs,
  saveAuthorizedGoogleDomains,
  saveOAuthSettings,
  setPasswordlessAuth,
  suspendUnlinkedUsers,
} from '@/api/client'
import type {
  AccessCodeView,
  OAuthSettingsView,
  PasswordlessStatus,
} from '@/api/client'

interface ProviderPanelProps {
  name: string
  icon: React.ReactNode
  clientId: string
  configured: boolean
  instructionsUrl: string
  instructionsLabel: string
  onSave: (clientId: string, clientSecret: string) => Promise<unknown>
  // Redirect URI Nimbus will send to this IdP. Empty when the backend
  // resolver isn't wired (older builds) — the panel just hides the hint.
  redirectURI?: string
  // What the IdP's console calls this field, so the hint copy matches.
  redirectURILabel?: string
  // 'cloud_tunnel' | 'app_url' | 'request_host' | '' — chooses which
  // sub-line ("from Gopher self-tunnel" / "from APP_URL" / etc.) to show.
  redirectURISource?: OAuthSettingsView['redirect_uri_source']
  // Warning text when the resolved host is unusable (loopback / raw IP).
  redirectURIWarning?: string
  children?: React.ReactNode
}

function ProviderPanel({
  name,
  icon,
  clientId: initialClientId,
  configured,
  instructionsUrl,
  instructionsLabel,
  onSave,
  redirectURI,
  redirectURILabel,
  redirectURISource,
  redirectURIWarning,
  children,
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

      {redirectURI && (
        <RedirectURIHint
          uri={redirectURI}
          fieldLabel={redirectURILabel ?? 'Redirect URI'}
          source={redirectURISource}
          warning={redirectURIWarning}
        />
      )}

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

      {children && (
        <div style={{ paddingTop: 18, borderTop: '1px solid var(--line)' }}>
          {children}
        </div>
      )}
    </div>
  )
}

// RedirectURIHint renders a read-only "paste this into the IdP console"
// field with a copy button + a one-line provenance hint and an optional
// warning when the resolved host is something Google would reject.
function RedirectURIHint({
  uri,
  fieldLabel,
  source,
  warning,
}: {
  uri: string
  fieldLabel: string
  source: OAuthSettingsView['redirect_uri_source']
  warning?: string
}) {
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(uri)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard can fail in non-HTTPS / iframe contexts — silently swallow;
      // user can still triple-click + Cmd/Ctrl-C the visible value.
    }
  }
  return (
    <div className="n-field" style={{ gap: 6 }}>
      <label className="n-label">{fieldLabel}</label>
      <div style={{ display: 'flex', gap: 8 }}>
        <input
          className="n-input"
          type="text"
          readOnly
          value={uri}
          onFocus={(e) => e.currentTarget.select()}
          style={{ flex: 1, fontFamily: 'Geist Mono, monospace', fontSize: 12 }}
        />
        <button
          type="button"
          className="n-btn"
          onClick={handleCopy}
          style={{ padding: '0 12px', fontSize: 12, display: 'inline-flex', alignItems: 'center', gap: 6 }}
          aria-label={`Copy ${fieldLabel}`}
        >
          <CopyIcon />
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.5 }}>
        Register this exact value as the {fieldLabel.toLowerCase()} on your OAuth app.{' '}
        {source === 'cloud_tunnel'
          ? 'Resolved from the Gopher self-tunnel.'
          : source === 'app_url'
            ? 'Resolved from APP_URL.'
            : source === 'request_host'
              ? 'Inferred from this browser session — set APP_URL or finish the Gopher self-bootstrap to make it stable.'
              : ''}
      </p>
      {warning && (
        <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--err)', lineHeight: 1.5 }}>
          {warning}
        </p>
      )}
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

function GoogleDomainsSection() {
  const [open, setOpen] = useState(false)
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
    <details
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary
        style={{
          cursor: 'pointer',
          listStyle: 'none',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 10,
          userSelect: 'none',
          padding: '4px 0',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>
            Authorized Google domains
          </span>
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
        <span
          aria-hidden="true"
          style={{ fontSize: 16, color: 'var(--ink-mute)', lineHeight: 1 }}
        >
          {open ? '▾' : '▸'}
        </span>
      </summary>

      <div style={{ marginTop: 16, display: 'flex', flexDirection: 'column', gap: 18 }}>
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
    </details>
  )
}

function GitHubOrgsSection() {
  const [open, setOpen] = useState(false)
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
    <details
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary
        style={{
          cursor: 'pointer',
          listStyle: 'none',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 10,
          userSelect: 'none',
          padding: '4px 0',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>
            Authorized GitHub organizations
          </span>
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
        <span
          aria-hidden="true"
          style={{ fontSize: 16, color: 'var(--ink-mute)', lineHeight: 1 }}
        >
          {open ? '▾' : '▸'}
        </span>
      </summary>

      <div style={{ marginTop: 16, display: 'flex', flexDirection: 'column', gap: 18 }}>
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
    </details>
  )
}


// SettingsSignIn — /settings/sign-in subpage. The single home for
// "who can sign in to this workspace": OAuth provider config, the
// access-code regenerate widget, the passwordless-sign-in toggle (with
// bulk-suspend / email-stragglers affordances), and the full account
// table. All four govern sign-in policy + membership, so they share a
// page rather than splitting across /users and /settings/sign-in like
// the previous iteration did.
//
// The page stacks: providers + passwordless summary, access code panel,
// then the accounts table. Every helper for the OAuth + access-code
// surfaces lives in this file; the accounts table lives in
// components/UsersTable.tsx since it's the one piece a future page
// (e.g. an audit-log surface) might want to reuse.
//
// editingProvider tracks which (if any) per-provider modal is open.
// Using a discrete identifier rather than a boolean lets the modal
// render the right provider's panel without the parent juggling two
// flags.
type EditingProvider = 'google' | 'github' | null

export default function SettingsSignIn() {
  const [settings, setSettings] = useState<OAuthSettingsView | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [editingProvider, setEditingProvider] = useState<EditingProvider>(null)
  // refreshTick lets ProvidersSummary's bulk-suspend / email-stragglers
  // actions tell the live UsersTable to re-fetch (and vice-versa: a
  // suspend/promote/delete from the table updates the straggler counts).
  const [refreshTick, setRefreshTick] = useState(0)
  const refreshAll = useCallback(() => setRefreshTick((t) => t + 1), [])

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
    <div style={{ display: 'flex', flexDirection: 'column', gap: 18 }}>
      <div>
        <h2 style={{ fontSize: 22, margin: '0 0 4px', fontWeight: 500 }}>
          Sign-in & access
        </h2>
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)' }}>
          OAuth providers, access code, the passwordless-sign-in toggle, and the
          full account list.
        </p>
      </div>

      {loadError && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{loadError}</p>
      )}

      <ProvidersSummary
        settings={settings}
        onEdit={(p) => setEditingProvider(p)}
        refreshTick={refreshTick}
        onMutated={refreshAll}
      />
      <AccessCodePanel />
      <UsersTable refreshTick={refreshTick} onMutated={refreshAll} />

      {editingProvider && settings && (
        <OAuthProviderModal
          provider={editingProvider}
          settings={settings}
          onClose={() => setEditingProvider(null)}
          onSaveGitHub={handleSaveGitHub}
          onSaveGoogle={handleSaveGoogle}
        />
      )}
    </div>
  )
}


// ProvidersSummary is the compact replacement for the old big OAuth
// section. Shows a one-line status for Google and GitHub plus a pencil
// button per row. Pressing a pencil opens a modal scoped to *that*
// provider — no shared "Configure" surface, since most operators only
// touch one provider per visit.
// ProvidersSummary now also hosts the passwordless-sign-in toggle —
// it's the same conceptual scope (sign-in providers + which sign-in
// surfaces are exposed on the login page), so giving it its own card
// was overkill. The toggle and bulk-suspend live in a divided second
// section so the OAuth client config stays the visual anchor.
function ProvidersSummary({
  settings,
  onEdit,
  refreshTick,
  onMutated,
}: {
  settings: OAuthSettingsView | null
  onEdit: (provider: 'google' | 'github') => void
  refreshTick: number
  onMutated: () => void
}) {
  const [status, setStatus] = useState<PasswordlessStatus | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    getPasswordlessStatus()
      .then(setStatus)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
  }, [refreshTick])

  const toggle = async () => {
    if (!status) return
    setError(null)
    setBusy(true)
    try {
      const next = await setPasswordlessAuth(!status.passwordless_goal)
      setStatus(next)
      onMutated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  const bulkSuspend = async () => {
    setError(null)
    setBusy(true)
    try {
      const { suspended } = await suspendUnlinkedUsers()
      onMutated()
      if (suspended === 0) {
        setError('No users needed suspending.')
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  const emailUnlinked = async () => {
    setError(null)
    setBusy(true)
    try {
      const r = await emailUnlinkedUsers()
      // Surface the result in the panel's existing status slot — the
      // bulk-action button reuses the suspend-result text style so we
      // don't introduce a third success/failure UI per panel.
      const failuresSummary =
        r.failed > 0 && r.failures && r.failures.length > 0
          ? ` Failures: ${r.failures.slice(0, 3).join('; ')}${r.failures.length > 3 ? `; +${r.failures.length - 3} more` : ''}`
          : ''
      setError(`Sent ${r.sent}${r.failed > 0 ? `, failed ${r.failed}.${failuresSummary}` : '.'}`)
      onMutated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Sign-in providers
      </span>
      <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
        OAuth client IDs + the access-code bypass lists (authorized Google
        domains, GitHub orgs). Click the pencil to edit one.
      </p>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        <ProviderRow
          icon={<GoogleIcon size={16} />}
          name="Google"
          configured={settings?.google_configured}
          onEdit={() => onEdit('google')}
        />
        <ProviderRow
          icon={<GithubIcon size={16} />}
          name="GitHub"
          configured={settings?.github_configured}
          onEdit={() => onEdit('github')}
        />
      </div>

      <div style={{ height: 1, background: 'var(--line)', margin: '4px 0' }} />

      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
          Passwordless sign-in
        </span>
        {status?.passwordless_goal && (
          <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
            <span className="n-pill-dot" />
            active
          </span>
        )}
      </div>

      {status && (
        <label
          style={{
            display: 'flex',
            alignItems: 'flex-start',
            gap: 10,
            padding: '10px 12px',
            border: '1px solid var(--line)',
            borderRadius: 10,
            cursor: busy ? 'wait' : 'pointer',
            background: status.passwordless_goal ? 'rgba(20,18,28,0.05)' : 'rgba(20,18,28,0.02)',
          }}
        >
          <input
            type="checkbox"
            checked={status.passwordless_goal}
            disabled={busy}
            onChange={toggle}
            style={{ marginTop: 3 }}
          />
          <span>
            <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
              Require OAuth sign-in
            </span>
            <span style={{ display: 'block', fontSize: 12, color: 'var(--ink-body)', lineHeight: 1.5, marginTop: 2 }}>
              {status.passwordless_goal
                ? 'Password form is hidden on the sign-in page.'
                : status.stragglers === 0
                  ? 'Every active user has OAuth linked. Toggle on to hide the password form.'
                  : `${status.stragglers} active user${status.stragglers === 1 ? '' : 's'} still depend${status.stragglers === 1 ? 's' : ''} on password sign-in. Suspend or delete them before enabling.`}
            </span>
          </span>
        </label>
      )}

      {status && !status.passwordless_goal && status.stragglers > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          <button
            type="button"
            className="n-btn"
            onClick={bulkSuspend}
            disabled={busy}
            style={{ fontSize: 12, padding: '6px 12px', height: 32 }}
            title="Suspends every active password-only user so you can flip the toggle. They keep their data and can be unsuspended later."
          >
            {busy ? 'Working…' : `Suspend ${status.stragglers} unlinked user${status.stragglers === 1 ? '' : 's'}`}
          </button>
          {/* Email-stragglers button. Enabled once SMTP is configured
              + enabled on /email; click mints magic-link tokens and
              sends each unlinked user a recovery email. Synchronous —
              the request waits on N SMTP roundtrips, but client and
              server both run with a generous timeout so a small batch
              completes in a single shot. */}
          <button
            type="button"
            className="n-btn"
            disabled={busy || !status.smtp_ready}
            onClick={emailUnlinked}
            style={{ fontSize: 12, padding: '6px 12px', height: 32, cursor: status.smtp_ready ? 'pointer' : 'not-allowed' }}
            title={
              status.smtp_ready
                ? `Sends a magic-link recovery email to each unlinked user. Each link expires in 24h.`
                : 'SMTP not configured — set up Email in the Control Panel to enable.'
            }
          >
            {busy ? 'Sending…' : `Email ${status.stragglers} unlinked user${status.stragglers === 1 ? '' : 's'}`}
          </button>
        </div>
      )}

      {error && <p style={{ margin: 0, fontSize: 12, color: 'var(--err)' }}>{error}</p>}
    </div>
  )
}

function ProviderRow({
  icon,
  name,
  configured,
  onEdit,
}: {
  icon: React.ReactNode
  name: string
  configured: boolean | undefined
  onEdit: () => void
}) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '8px 12px', background: 'rgba(20,18,28,0.03)', border: '1px solid var(--line)', borderRadius: 8, gap: 8 }}>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8, fontSize: 13, color: 'var(--ink)', flex: 1, minWidth: 0 }}>
        {icon}
        {name}
      </span>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
        {configured ? (
          <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
            <span className="n-pill-dot" />
            configured
          </span>
        ) : (
          <span
            className="n-pill"
            style={{
              fontSize: 10,
              color: 'var(--ink-mute)',
              background: 'rgba(20,18,28,0.04)',
              border: '1px solid var(--line)',
            }}
          >
            not configured
          </span>
        )}
        <button
          type="button"
          onClick={onEdit}
          aria-label={`Edit ${name} sign-in provider`}
          title={`Edit ${name}`}
          style={{
            background: 'transparent',
            border: '1px solid var(--line)',
            borderRadius: 6,
            padding: '4px 6px',
            cursor: 'pointer',
            color: 'var(--ink-mute)',
            display: 'inline-flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          <PencilIcon />
        </button>
      </span>
    </div>
  )
}

// PencilIcon is a small inline edit glyph, matching the visual weight
// of the existing icons in this file (Eye, Copy, Refresh).
function PencilIcon() {
  return (
    <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 20h9" />
      <path d="M16.5 3.5a2.121 2.121 0 1 1 3 3L7 19l-4 1 1-4Z" />
    </svg>
  )
}


// OAuthProviderModal renders one provider's full panel — picker + bypass
// list — scoped to the provider the user clicked. Keeps the existing
// ProviderPanel + Section components unchanged; only the framing shell
// moved.
function OAuthProviderModal({
  provider,
  settings,
  onClose,
  onSaveGitHub,
  onSaveGoogle,
}: {
  provider: 'google' | 'github'
  settings: OAuthSettingsView
  onClose: () => void
  onSaveGitHub: (clientId: string, clientSecret: string) => Promise<unknown>
  onSaveGoogle: (clientId: string, clientSecret: string) => Promise<unknown>
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])

  const isGoogle = provider === 'google'
  const titleName = isGoogle ? 'Google' : 'GitHub'

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={`${titleName} sign-in provider`}
      onClick={onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 720, maxHeight: 'calc(100vh - 2rem)', overflowY: 'auto', padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 16, marginBottom: 18 }}>
          <div>
            <div className="eyebrow">Sign-in provider</div>
            <h3 style={{ fontSize: 22, margin: '4px 0 0' }}>{titleName}</h3>
            <p style={{ margin: '6px 0 0', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
              OAuth client ID/secret and the access-code bypass list for {titleName}.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            style={{ background: 'none', border: 'none', fontSize: 22, lineHeight: 1, color: 'var(--ink-mute)', cursor: 'pointer', padding: 4, marginRight: -4, marginTop: -4 }}
          >
            ×
          </button>
        </div>

        {isGoogle ? (
          <ProviderPanel
            name="Google"
            icon={<GoogleIcon size={20} />}
            clientId={settings.google_client_id}
            configured={settings.google_configured}
            instructionsUrl="https://console.cloud.google.com/apis/credentials"
            instructionsLabel="Google Cloud Console"
            onSave={onSaveGoogle}
            redirectURI={settings.google_redirect_uri}
            redirectURILabel="Authorized redirect URI"
            redirectURISource={settings.redirect_uri_source}
            redirectURIWarning={settings.redirect_uri_warning}
          >
            <GoogleDomainsSection />
          </ProviderPanel>
        ) : (
          <ProviderPanel
            name="GitHub"
            icon={<GithubIcon size={20} />}
            clientId={settings.github_client_id}
            configured={settings.github_configured}
            instructionsUrl="https://github.com/settings/applications/new"
            instructionsLabel="github.com/settings/applications/new"
            onSave={onSaveGitHub}
            redirectURI={settings.github_callback_url}
            redirectURILabel="Authorization callback URL"
            redirectURISource={settings.redirect_uri_source}
            redirectURIWarning={settings.redirect_uri_warning}
          >
            <GitHubOrgsSection />
          </ProviderPanel>
        )}
      </div>
    </div>,
    document.body,
  )
}
