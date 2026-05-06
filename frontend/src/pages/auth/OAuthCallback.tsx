import { useEffect } from 'react'
import { useNavigate, useSearchParams, Link } from 'react-router-dom'
import { useAuth } from '@/hooks/useAuth'
import { GithubIcon, GoogleIcon } from '@/components/nimbus'

// OAuthCallback is the post-redirect landing page for /api/auth/{github,
// google}/callback. By the time this React route renders, the backend
// has already finished the OAuth dance and set the session cookie — so
// the success path is just a redirect, no work to display.
//
// Earlier this page staggered a fake 4-step checklist ("Verifying…",
// "Exchanging code…", "Resolving identity…", "Issuing session cookie")
// over ~2 s of setTimeouts. None of those steps were actually
// happening; they were UI theater for work that had completed before
// the redirect. Removed.
//
// The error path stays — when the backend redirects with ?error=…,
// surfacing the human-readable message + a "Back to sign in" button is
// the only useful thing this route can do.

type ProviderName = 'github' | 'google'

const PROVIDER_LABEL: Record<ProviderName, string> = {
  github: 'GitHub',
  google: 'Google',
}

const ERROR_MESSAGES: Record<string, string> = {
  invalid_state:         'OAuth state mismatch — possible CSRF attempt. Please try again.',
  access_denied:         'Authorization was cancelled.',
  exchange_failed:       'Failed to exchange the authorization code.',
  user_failed:           'Could not retrieve your account details.',
  session_failed:        'Account verified, but session creation failed. Please try again.',
  missing_code:          'Authorization code was missing from the callback.',
  domain_not_authorized: 'Your email domain is not authorized for sign-up. Contact your administrator to request access.',
  org_not_authorized:    'Your GitHub account is not a member of an authorized organization. Contact your administrator to request access.',
  account_suspended:     'Your account is suspended. Contact your administrator.',
}

export default function OAuthCallback() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const { user } = useAuth()

  const error = params.get('error')
  const providerKey = (params.get('provider') ?? 'github') as ProviderName
  const providerLabel = PROVIDER_LABEL[providerKey] ?? PROVIDER_LABEL.github

  // Success path: redirect immediately. Admin lands on /admin, regular
  // user lands on / (their VMs / provision form). No animation, no
  // pretend-progress — the backend already did the work.
  useEffect(() => {
    if (error) return
    navigate(user?.is_admin ? '/admin' : '/', { replace: true })
  }, [error, user, navigate])

  // Render nothing while the success-path redirect runs — it's
  // synchronous on next tick, so a flash of empty content is preferable
  // to a flash of "Signing you in…" copy that's stale by the time the
  // user reads it.
  if (!error) return null

  return (
    <div style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column', padding: '20px 24px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <div className="brand-mark" />
        <span style={{ fontFamily: 'var(--font-display)', fontSize: 17, color: 'var(--ink)', letterSpacing: '-0.02em' }}>
          Nimbus
        </span>
      </div>

      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <div className="glass" style={{ width: '100%', maxWidth: 440, padding: '36px 40px' }}>
          <div style={{ display: 'flex', gap: 20, alignItems: 'flex-start', marginBottom: 24 }}>
            <div
              style={{
                width: 56, height: 56, borderRadius: 14,
                display: 'flex', alignItems: 'center', justifyContent: 'center',
                flexShrink: 0,
                background: providerKey === 'google' ? '#fff' : 'var(--ink)',
                border: providerKey === 'google' ? '1px solid rgba(20,18,28,0.1)' : 'none',
                color: providerKey === 'google' ? 'inherit' : '#fff',
              }}
            >
              {providerKey === 'google' ? <GoogleIcon size={26} /> : <GithubIcon size={28} />}
            </div>
            <div>
              <h1 className="n-display" style={{ fontSize: 22, margin: '0 0 4px', lineHeight: 1.2 }}>
                Sign-in failed
              </h1>
              <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
                {ERROR_MESSAGES[error] ?? `${providerLabel} sign-in could not complete.`}
              </p>
            </div>
          </div>

          <Link
            to="/login"
            className="n-btn n-btn-primary n-btn-block"
            style={{ display: 'flex', textDecoration: 'none' }}
          >
            Back to sign in
          </Link>
        </div>
      </div>
    </div>
  )
}
