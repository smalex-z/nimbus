import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import { changePassword, getAccount, getProviders } from '@/api/client'
import type { AccountView, OAuthProviders } from '@/api/client'

// Account — a personal page reachable from the user dropdown by every
// signed-in user (admin or member). Renders the read-only profile and
// the Connect Google / Connect GitHub buttons. Linking goes through
// /api/auth/{provider}/link, which sets a link-intent cookie before the
// OAuth dance — the same callback then attaches the identity to the
// current user instead of creating/finding by email.
//
// After a successful link the OAuth callback redirects here with
// `?linked=google` or `?linked=github`; we surface a small banner and
// refresh the account view so the Connect button flips to Connected.
export default function Account() {
  const [account, setAccount] = useState<AccountView | null>(null)
  const [providers, setProviders] = useState<OAuthProviders | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [params, setParams] = useSearchParams()
  const justLinked = params.get('linked')

  const reload = () => {
    Promise.all([getAccount(), getProviders()])
      .then(([a, p]) => {
        setAccount(a)
        setProviders(p)
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed to load'))
  }

  useEffect(() => {
    reload()
    // If we landed here from a successful link callback, drop the
    // query string after a beat so a refresh doesn't keep showing the
    // banner. The reload above will pick up the new state.
    if (justLinked) {
      const t = setTimeout(() => {
        params.delete('linked')
        setParams(params, { replace: true })
      }, 4000)
      return () => clearTimeout(t)
    }
    return undefined
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const linkedBanner = useMemo(() => {
    if (!justLinked) return null
    const label = justLinked === 'google' ? 'Google' : justLinked === 'github' ? 'GitHub' : null
    if (!label) return null
    return `${label} account linked.`
  }, [justLinked])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Account
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Your profile and the sign-in providers linked to this account.
        </p>
      </div>

      {linkedBanner && (
        <div
          className="n-pill n-pill-ok"
          style={{ alignSelf: 'flex-start', fontSize: 12, padding: '6px 12px' }}
        >
          <span className="n-pill-dot" />
          {linkedBanner}
        </div>
      )}

      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}

      {account === null && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      )}

      {account && (
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
          <div className="lg:col-span-2 flex flex-col gap-6">
            <ProfileCard account={account} />
            <LinkedProvidersCard account={account} providers={providers} />
            <PasswordCard hasPassword={account.has_password} onUpdated={reload} />
          </div>
        </div>
      )}
    </div>
  )
}

function ProfileCard({ account }: { account: AccountView }) {
  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Profile
      </span>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <ProfileRow label="Display name" value={account.name || '—'} />
        <ProfileRow label="Email" value={account.email} mono />
        <ProfileRow
          label="Role"
          value={account.is_admin ? 'admin' : 'member'}
        />
      </div>
    </div>
  )
}

function ProfileRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 12 }}>
      <span style={{ fontSize: 12, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em' }}>
        {label}
      </span>
      <span
        style={{
          fontSize: 13,
          color: 'var(--ink)',
          fontFamily: mono ? 'Geist Mono, monospace' : undefined,
          textAlign: 'right',
        }}
      >
        {value}
      </span>
    </div>
  )
}

function LinkedProvidersCard({
  account,
  providers,
}: {
  account: AccountView
  providers: OAuthProviders | null
}) {
  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Linked sign-in providers
      </span>
      <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
        Connect Google or GitHub so you can sign in with one click. Linking
        is required if your admin moves the workspace to OAuth-only sign-in.
      </p>

      <ProviderLinkRow
        icon={<GoogleIcon size={18} />}
        name="Google"
        connected={account.google_connected}
        configured={Boolean(providers?.google)}
        href="/api/auth/google/link"
      />
      <ProviderLinkRow
        icon={<GithubIcon size={18} />}
        name="GitHub"
        connected={account.github_connected}
        configured={Boolean(providers?.github)}
        href="/api/auth/github/link"
      />

      {account.has_password && (
        <p style={{ margin: '4px 0 0', fontSize: 11, color: 'var(--ink-mute)' }}>
          You also have a password set on this account.
        </p>
      )}
    </div>
  )
}

function ProviderLinkRow({
  icon,
  name,
  connected,
  configured,
  href,
}: {
  icon: React.ReactNode
  name: string
  connected: boolean
  // configured tells us whether the admin has filled in OAuth client
  // ID/secret for this provider. Without that, the Connect button
  // can't lead anywhere useful.
  configured: boolean
  href: string
}) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '10px 14px',
        background: 'rgba(20,18,28,0.03)',
        border: '1px solid var(--line)',
        borderRadius: 10,
      }}
    >
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10, fontSize: 14, color: 'var(--ink)' }}>
        {icon}
        {name}
      </span>
      {connected ? (
        <span className="n-pill n-pill-ok" style={{ fontSize: 11 }}>
          <span className="n-pill-dot" />
          connected
        </span>
      ) : configured ? (
        // Direct browser navigation rather than an axios call: the
        // OAuth dance must hit the provider via top-level redirects.
        <a
          href={href}
          className="n-btn"
          style={{ fontSize: 12, padding: '6px 12px', textDecoration: 'none' }}
        >
          Connect
        </a>
      ) : (
        <span
          className="n-pill"
          style={{
            fontSize: 11,
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
          title={`${name} OAuth isn't configured on this Nimbus instance yet — ask an admin to add the client ID/secret.`}
        >
          provider not configured
        </span>
      )}
    </div>
  )
}

// PasswordCard renders the self-service set-or-change-password form. Two
// variants driven by hasPassword:
//   - true:  current + new + confirm fields. Server requires old to match.
//   - false: new + confirm only. OAuth-only accounts adding a password.
//
// On success we call onUpdated() so the parent reloads the AccountView and
// the card re-renders with the "true" variant — useful so a first-time
// setter sees the "current password" field appear without a manual refresh.
function PasswordCard({ hasPassword, onUpdated }: { hasPassword: boolean; onUpdated: () => void }) {
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  const newTooShort = newPassword.length > 0 && newPassword.length < 8
  const mismatch = confirm.length > 0 && confirm !== newPassword
  const canSubmit =
    newPassword.length >= 8 &&
    confirm === newPassword &&
    (!hasPassword || oldPassword.length > 0) &&
    !saving

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    if (!canSubmit) return
    try {
      setSaving(true)
      await changePassword({
        old_password: hasPassword ? oldPassword : undefined,
        new_password: newPassword,
      })
      setOldPassword('')
      setNewPassword('')
      setConfirm('')
      setSaved(true)
      setTimeout(() => setSaved(false), 3000)
      onUpdated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update password')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        {hasPassword ? 'Change password' : 'Set a password'}
      </span>
      <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
        {hasPassword
          ? 'Update the password on your account. You stay signed in on this device — only the stored credential changes.'
          : 'Add a password so you can sign in without an OAuth provider. Useful as a fallback when your provider is unreachable or your admin moves the workspace off OAuth-only sign-in.'}
      </p>

      <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        {hasPassword && (
          <div className="n-field">
            <label className="n-label" htmlFor="account-old-password">Current password</label>
            <input
              id="account-old-password"
              className="n-input"
              type="password"
              autoComplete="current-password"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
            />
          </div>
        )}
        <div className="n-field">
          <label className="n-label" htmlFor="account-new-password">New password</label>
          <input
            id="account-new-password"
            className="n-input"
            type="password"
            autoComplete="new-password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
          {newTooShort && (
            <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--err)' }}>
              At least 8 characters.
            </p>
          )}
        </div>
        <div className="n-field">
          <label className="n-label" htmlFor="account-confirm-password">Confirm new password</label>
          <input
            id="account-confirm-password"
            className="n-input"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          {mismatch && (
            <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--err)' }}>
              Doesn't match the new password.
            </p>
          )}
        </div>

        {error && (
          <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
        )}

        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button
            type="submit"
            className="n-btn n-btn-primary"
            disabled={!canSubmit}
            style={{ minWidth: 100 }}
          >
            {saving ? 'Saving…' : hasPassword ? 'Update password' : 'Set password'}
          </button>
          {saved && (
            <span style={{ fontSize: 13, color: 'var(--ok)' }}>
              {hasPassword ? 'Password updated.' : 'Password set.'}
            </span>
          )}
        </div>
      </form>
    </div>
  )
}
