import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import {
  deleteUser,
  getAccessCode,
  getAuthorizedGitHubOrgs,
  getAuthorizedGoogleDomains,
  getOAuthSettings,
  listUsers,
  promoteUser,
  regenerateAccessCode,
  saveAuthorizedGitHubOrgs,
  saveAuthorizedGoogleDomains,
  saveOAuthSettings,
} from '@/api/client'
import type {
  AccessCodeView,
  OAuthSettingsView,
  UserManagementView,
} from '@/api/client'
import NavDropdown from '@/components/ui/NavDropdown'
import { useAuth } from '@/hooks/useAuth'

interface ProviderPanelProps {
  name: string
  icon: React.ReactNode
  clientId: string
  configured: boolean
  instructionsUrl: string
  instructionsLabel: string
  onSave: (clientId: string, clientSecret: string) => Promise<unknown>
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


// Users — admin-facing user management + access policy.
//
// Page layout, top-down:
//   1. Access code panel + Users table (the things admins reach for daily).
//   2. Sign-in providers summary card (Google + GitHub status pills, plus
//      a "Configure providers" button that opens the full OAuth + bypass
//      configuration in a modal).
//
// The OAuth provider panels were the bulk of the old /settings page; they
// stay reachable but live behind a click since most operators only touch
// them once at setup. Pushing them down/behind a modal keeps the access
// code + users-list — which admins actually look at — above the fold.
// editingProvider tracks which (if any) per-provider modal is open. Using
// a discrete identifier rather than a boolean lets the modal render the
// right provider's panel without the parent juggling two flags.
type EditingProvider = 'google' | 'github' | null

export default function Users() {
  const [settings, setSettings] = useState<OAuthSettingsView | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [editingProvider, setEditingProvider] = useState<EditingProvider>(null)

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
          Users
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Every account that has signed up, plus the access code and sign-in
          providers gating new sign-ins.
        </p>
      </div>

      {loadError && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{loadError}</p>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          <UsersTable />
        </div>
        <div className="lg:col-span-1 flex flex-col gap-6">
          <AccessCodePanel />
          <ProvidersSummary
            settings={settings}
            onEdit={(p) => setEditingProvider(p)}
          />
        </div>
      </div>

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

// UsersTable renders every account the admin can see. Sorted server-side
// by created_at desc, so the most recent sign-up shows first. Verified
// status follows the live policy (admin / authorized domain / org / code
// match) so the column reflects what would happen on the user's next
// request, not a snapshot at sign-up.
// pendingAction tracks which row + modal are currently open. Single
// state object so we can't accidentally show both modals at once.
type PendingAction =
  | { kind: 'promote'; user: UserManagementView }
  | { kind: 'delete'; user: UserManagementView }
  | null

function UsersTable() {
  const { user: me } = useAuth()
  const [rows, setRows] = useState<UserManagementView[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction>(null)

  const reload = () => {
    listUsers()
      .then(setRows)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
  }

  useEffect(() => {
    reload()
  }, [])

  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Accounts
        </span>
        {rows !== null && (
          <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
            {rows.length} {rows.length === 1 ? 'user' : 'users'}
          </span>
        )}
      </div>

      {rows === null && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      )}
      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}
      {rows !== null && rows.length === 0 && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          No accounts yet.
        </p>
      )}
      {rows !== null && rows.length > 0 && (
        <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
          <table className="w-full text-left" style={{ fontSize: 13, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ color: 'var(--ink-mute)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Name</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Email</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Joined</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Sign-in</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Status</th>
                <th style={{ padding: '8px 8px', fontWeight: 500, width: 1 }} aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => (
                <tr
                  key={u.id}
                  style={{ borderTop: '1px solid var(--line)' }}
                >
                  <td style={{ padding: '10px 8px', color: 'var(--ink)', fontWeight: 500 }}>
                    {u.name || <span style={{ color: 'var(--ink-mute)' }}>—</span>}
                  </td>
                  <td style={{ padding: '10px 8px', color: 'var(--ink-body)', fontFamily: 'Geist Mono, monospace', fontSize: 12 }}>
                    {u.email}
                  </td>
                  <td style={{ padding: '10px 8px', color: 'var(--ink-body)', whiteSpace: 'nowrap' }}>
                    {formatJoined(u.created_at)}
                  </td>
                  <td style={{ padding: '10px 8px' }}>
                    <ProviderChips providers={u.providers} />
                  </td>
                  <td style={{ padding: '10px 8px' }}>
                    <UserStatusPills user={u} />
                  </td>
                  <td style={{ padding: '10px 8px', textAlign: 'right' }}>
                    <UserRowActions
                      user={u}
                      isSelf={me?.id === u.id}
                      onPromote={() => setPending({ kind: 'promote', user: u })}
                      onDelete={() => setPending({ kind: 'delete', user: u })}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {pending?.kind === 'promote' && (
        <PromoteUserModal
          user={pending.user}
          onClose={() => setPending(null)}
          onPromoted={() => {
            setPending(null)
            reload()
          }}
        />
      )}
      {pending?.kind === 'delete' && (
        <DeleteUserModal
          user={pending.user}
          onClose={() => setPending(null)}
          onDeleted={() => {
            setPending(null)
            reload()
          }}
        />
      )}
    </div>
  )
}

// UserRowActions shows the per-row "..." menu. The trigger button is
// always rendered (so column widths don't jump when self-row vs not),
// but its menu is empty-only when the row is the requester themselves —
// admins can't promote or delete themselves through this UI.
function UserRowActions({
  user,
  isSelf,
  onPromote,
  onDelete,
}: {
  user: UserManagementView
  isSelf: boolean
  onPromote: () => void
  onDelete: () => void
}) {
  if (isSelf) {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  // NavDropdown's open state is internal — clicks on items inside its panel
  // are ignored by its document-mousedown close handler (panel.contains
  // returns true). For an *action* menu we want the panel to dismiss as
  // soon as the user picks something, so we synthesize a mousedown on
  // document before invoking the handler. The synthetic event's target is
  // outside the panel, which trips NavDropdown's existing close path.
  // Without this the menu stays mounted at z-1000 and visibly floats
  // above the modal backdrop.
  const dismissAndDo = (fn: () => void) => () => {
    document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    fn()
  }
  return (
    <NavDropdown
      placement="bottom-end"
      triggerOn="click"
      triggerClassName="inline-flex items-center justify-center rounded-md border border-line-2 bg-white/85 hover:border-ink transition-colors"
      panelClassName="rounded-lg border border-line bg-white py-1 min-w-[180px] shadow-lg"
      trigger={
        <span style={{ display: 'inline-flex', width: 28, height: 28, alignItems: 'center', justifyContent: 'center', color: 'var(--ink-2)', fontSize: 16, fontWeight: 700 }}>
          ⋯
        </span>
      }
    >
      {!user.is_admin && (
        <button
          type="button"
          onClick={dismissAndDo(onPromote)}
          className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
        >
          Promote to admin
        </button>
      )}
      {user.is_admin && (
        <span className="block w-full text-left px-3 py-1.5 text-[13px] text-ink-3 cursor-default">
          Already an admin
        </span>
      )}
      <div className="my-1 border-t border-line" />
      <button
        type="button"
        onClick={dismissAndDo(onDelete)}
        className="block w-full text-left px-3 py-1.5 text-[13px] text-bad hover:bg-[rgba(184,55,55,0.06)] cursor-pointer"
      >
        Delete user…
      </button>
    </NavDropdown>
  )
}

// PromoteUserModal collects the requesting admin's password and POSTs
// to /api/users/:id/promote. The password input is the same gate the
// backend enforces — the UI doesn't pre-validate it (would leak whether
// the password is right against the wrong account); the server returns
// 401 on mismatch and we surface that.
function PromoteUserModal({
  user,
  onClose,
  onPromoted,
}: {
  user: UserManagementView
  onClose: () => void
  onPromoted: () => void
}) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose, busy])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await promoteUser(user.id, password)
      onPromoted()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Promote failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={`Promote ${user.name || user.email} to admin`}
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 460, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Promote to admin</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
          {user.name || user.email}
        </h3>
        <p style={{ margin: '0 0 18px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Grants full admin access — cluster observability, settings, and
          user management. Confirm with your password.
        </p>

        <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="n-field">
            <label className="n-label" htmlFor="promote-password">Your password</label>
            <input
              id="promote-password"
              className="n-input"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              autoFocus
            />
          </div>
          {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
          <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
            <button type="button" className="n-btn" onClick={onClose} disabled={busy}>
              Cancel
            </button>
            <button type="submit" className="n-btn n-btn-primary" disabled={busy || !password}>
              {busy ? 'Promoting…' : 'Promote'}
            </button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// DeleteUserModal is the destructive flow. The VM-disposition radio
// always shows even when the user has no VMs — it's the only safety
// step on this action and we want the operator to consciously choose,
// not have it auto-skipped because the count happened to be zero this
// minute. Dropping their VMs is *strictly* more destructive than
// dropping just the user record, so the default selection is "transfer".
function DeleteUserModal({
  user,
  onClose,
  onDeleted,
}: {
  user: UserManagementView
  onClose: () => void
  onDeleted: () => void
}) {
  const [vmAction, setVmAction] = useState<'transfer' | 'delete'>('transfer')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose, busy])

  const submit = async () => {
    setError(null)
    setBusy(true)
    try {
      await deleteUser(user.id, vmAction)
      onDeleted()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Delete failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={`Delete ${user.name || user.email}`}
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 480, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow" style={{ color: 'var(--err)' }}>Delete user</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
          {user.name || user.email}
        </h3>
        <p style={{ margin: '0 0 16px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Removes this user, their sessions, and (per the option below) their
          VMs and SSH keys. This cannot be undone.
        </p>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 10, marginBottom: 18 }}>
          <DispositionOption
            checked={vmAction === 'transfer'}
            onSelect={() => setVmAction('transfer')}
            title="Take ownership of their VMs"
            description="VMs and SSH keys are reparented to your account. They keep running; you'll see them on My machines."
          />
          <DispositionOption
            checked={vmAction === 'delete'}
            onSelect={() => setVmAction('delete')}
            title="Delete their VMs"
            description="VMs are destroyed on Proxmox and their SSH keys + GPU job history are removed. Slow if they own many VMs."
          />
        </div>

        {error && <p style={{ margin: '0 0 10px', fontSize: 13, color: 'var(--err)' }}>{error}</p>}

        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button type="button" className="n-btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className="n-btn"
            onClick={submit}
            disabled={busy}
            style={{ borderColor: 'var(--err)', color: 'var(--err)' }}
          >
            {busy
              ? 'Deleting…'
              : vmAction === 'transfer'
                ? 'Delete user, take their VMs'
                : 'Delete user and their VMs'}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function DispositionOption({
  checked,
  onSelect,
  title,
  description,
}: {
  checked: boolean
  onSelect: () => void
  title: string
  description: string
}) {
  return (
    <label
      style={{
        display: 'flex',
        gap: 10,
        alignItems: 'flex-start',
        padding: '12px 14px',
        border: `1px solid ${checked ? 'var(--ink)' : 'var(--line)'}`,
        background: checked ? 'rgba(20,18,28,0.04)' : 'rgba(20,18,28,0.02)',
        borderRadius: 10,
        cursor: 'pointer',
      }}
    >
      <input
        type="radio"
        name="vm-action"
        checked={checked}
        onChange={onSelect}
        style={{ marginTop: 3 }}
      />
      <span>
        <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
          {title}
        </span>
        <span style={{ display: 'block', fontSize: 12, color: 'var(--ink-body)', lineHeight: 1.5, marginTop: 2 }}>
          {description}
        </span>
      </span>
    </label>
  )
}

function ProviderChips({ providers }: { providers: string[] }) {
  if (!providers || providers.length === 0) {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  return (
    <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}>
      {providers.map((p) => (
        <span
          key={p}
          style={{
            fontSize: 10,
            fontFamily: 'Geist Mono, monospace',
            textTransform: 'uppercase',
            letterSpacing: '0.06em',
            padding: '2px 6px',
            borderRadius: 4,
            background: 'rgba(20,18,28,0.05)',
            border: '1px solid var(--line)',
            color: 'var(--ink-body)',
          }}
        >
          {p}
        </span>
      ))}
    </span>
  )
}

function UserStatusPills({ user }: { user: UserManagementView }) {
  return (
    <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
      {user.is_admin ? (
        <span
          className="font-mono"
          style={{
            fontSize: 10,
            fontWeight: 600,
            textTransform: 'uppercase',
            letterSpacing: '0.06em',
            padding: '2px 6px',
            borderRadius: 4,
            color: '#9a5c2e',
            background: 'rgba(248,175,130,0.15)',
            border: '1px solid rgba(248,175,130,0.4)',
          }}
        >
          admin
        </span>
      ) : null}
      {user.verified ? (
        <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
          <span className="n-pill-dot" />
          verified
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
          unverified
        </span>
      )}
    </span>
  )
}

// formatJoined renders a relative-ish "joined" string. Recent times show
// as "today" / "Xd ago"; anything older falls back to a short date.
function formatJoined(iso: string): string {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return '—'
  const ms = Date.now() - t
  const days = Math.floor(ms / 86_400_000)
  if (days < 1) return 'today'
  if (days < 30) return `${days}d ago`
  return new Date(t).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
}

// ProvidersSummary is the compact replacement for the old big OAuth
// section. Shows a one-line status for Google and GitHub plus a pencil
// button per row. Pressing a pencil opens a modal scoped to *that*
// provider — no shared "Configure" surface, since most operators only
// touch one provider per visit.
function ProvidersSummary({
  settings,
  onEdit,
}: {
  settings: OAuthSettingsView | null
  onEdit: (provider: 'google' | 'github') => void
}) {
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
          >
            <GitHubOrgsSection />
          </ProviderPanel>
        )}
      </div>
    </div>,
    document.body,
  )
}
