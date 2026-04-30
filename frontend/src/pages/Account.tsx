import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'
import { getAccount, getProviders } from '@/api/client'
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
