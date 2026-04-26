import { useEffect, useState } from 'react'
import { useNavigate, useSearchParams, Link } from 'react-router-dom'
import { GithubIcon, GoogleIcon, CheckIcon } from '@/components/nimbus'

interface Step {
  title: string
  detail: string
}

type ProviderName = 'github' | 'google'

interface ProviderConfig {
  label: string
  icon: JSX.Element
  steps: (login: string) => Step[]
}

const PROVIDERS: Record<ProviderName, ProviderConfig> = {
  github: {
    label: 'GitHub',
    icon: <GithubIcon size={28} />,
    steps: (login) => [
      { title: 'Verifying GitHub authorization',   detail: 'GET /api/auth/github/callback?code=…' },
      { title: 'Exchanging code for access token', detail: 'POST /login/oauth/access_token' },
      { title: 'Resolving user identity',          detail: `GET /user → @${login}` },
      { title: 'Issuing session cookie',           detail: 'Set-Cookie: nimbus_sid=…' },
    ],
  },
  google: {
    label: 'Google',
    icon: <GoogleIcon size={26} />,
    steps: (login) => [
      { title: 'Verifying Google authorization',   detail: 'GET /api/auth/google/callback?code=…' },
      { title: 'Exchanging code for access token', detail: 'POST /oauth2.googleapis.com/token' },
      { title: 'Resolving user identity',          detail: `GET /userinfo → ${login}` },
      { title: 'Issuing session cookie',           detail: 'Set-Cookie: nimbus_sid=…' },
    ],
  },
}

const ERROR_MESSAGES: Record<string, string> = {
  invalid_state:         'OAuth state mismatch — possible CSRF attempt. Please try again.',
  access_denied:         'Authorization was cancelled.',
  exchange_failed:       'Failed to exchange the authorization code.',
  user_failed:           'Could not retrieve your account details.',
  session_failed:        'Account verified, but session creation failed. Please try again.',
  missing_code:          'Authorization code was missing from the callback.',
  domain_not_authorized: 'Your email domain is not authorized for sign-up. Contact your administrator to request access.',
}

export default function OAuthCallback() {
  const [params] = useSearchParams()
  const navigate = useNavigate()

  const error = params.get('error')
  const login = params.get('login') ?? 'you'
  const providerKey = (params.get('provider') ?? 'github') as ProviderName
  const provider = PROVIDERS[providerKey] ?? PROVIDERS.github
  const steps = provider.steps(login)

  const [visibleCount, setVisibleCount] = useState(0)

  useEffect(() => {
    if (error) return
    const timers: ReturnType<typeof setTimeout>[] = []
    steps.forEach((_, i) => {
      timers.push(setTimeout(() => setVisibleCount(i + 1), 350 + i * 420))
    })
    timers.push(setTimeout(() => navigate('/', { replace: true }), 350 + steps.length * 420 + 600))
    return () => timers.forEach(clearTimeout)
  }, [error]) // eslint-disable-line react-hooks/exhaustive-deps

  const iconBg = providerKey === 'google'
    ? { background: '#fff', border: '1px solid rgba(20,18,28,0.1)', color: 'inherit' }
    : { background: 'var(--ink)', color: '#fff' }

  return (
    <div style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column', padding: '20px 24px' }}>
      {/* Top bar */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 'auto' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <div className="brand-mark" />
          <span style={{ fontFamily: 'var(--font-display)', fontSize: 17, color: 'var(--ink)', letterSpacing: '-0.02em' }}>
            Nimbus
          </span>
        </div>

        {!error && (
          <span style={{
            display: 'inline-flex', alignItems: 'center', gap: 7,
            padding: '5px 12px', borderRadius: 999,
            background: 'rgba(248,175,130,0.12)', border: '1px solid rgba(248,175,130,0.35)',
            fontSize: 12, fontWeight: 500, color: '#9a5c2e',
          }}>
            <span style={{
              width: 7, height: 7, borderRadius: '50%', background: '#F8AF82',
              animation: 'blink 1.2s ease-in-out infinite', display: 'inline-block',
            }} />
            signing you in
          </span>
        )}
      </div>

      {/* Card */}
      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <div className="glass" style={{ width: '100%', maxWidth: 440, padding: '36px 40px' }}>
          {/* Icon + heading */}
          <div style={{ display: 'flex', gap: 20, alignItems: 'flex-start', marginBottom: 32 }}>
            <div style={{
              width: 56, height: 56, borderRadius: 14,
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              flexShrink: 0, ...iconBg,
            }}>
              {provider.icon}
            </div>
            <div>
              <h1 className="n-display" style={{ fontSize: 22, margin: '0 0 4px', lineHeight: 1.2 }}>
                {error ? 'Something went wrong' : `Returning from ${provider.label}`}
              </h1>
              <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
                {error
                  ? (ERROR_MESSAGES[error] ?? 'An unexpected error occurred.')
                  : 'Hold on a second — finishing handshake.'}
              </p>
            </div>
          </div>

          {/* Steps */}
          {!error && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 18 }}>
              {steps.map((step, i) => (
                <div key={i} style={{
                  display: 'flex', gap: 14, alignItems: 'flex-start',
                  opacity: visibleCount > i ? 1 : 0,
                  transform: visibleCount > i ? 'translateY(0)' : 'translateY(6px)',
                  transition: 'opacity 0.35s ease, transform 0.35s ease',
                }}>
                  <div style={{
                    width: 22, height: 22, borderRadius: '50%',
                    background: 'rgba(31,122,77,0.12)', border: '1px solid rgba(31,122,77,0.25)',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    flexShrink: 0, color: 'var(--ok)', marginTop: 1,
                  }}>
                    <CheckIcon size={12} />
                  </div>
                  <div>
                    <p style={{ margin: 0, fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>
                      {step.title}
                    </p>
                    <p style={{ margin: 0, fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'var(--font-mono)', marginTop: 2 }}>
                      {step.detail}
                    </p>
                  </div>
                </div>
              ))}
            </div>
          )}

          {error && (
            <Link to="/login" className="n-btn n-btn-primary n-btn-block"
              style={{ display: 'flex', textDecoration: 'none', marginTop: 8 }}>
              Back to sign in
            </Link>
          )}
        </div>
      </div>

      {/* Footer */}
      <div style={{
        display: 'flex', justifyContent: 'space-between', alignItems: 'center',
        marginTop: 'auto', paddingTop: 20,
        fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'var(--font-mono)',
      }}>
        <span>handshake · ed25519 · TLS 1.3</span>
        <Link to="/login" style={{ color: 'var(--ink-mute)', textDecoration: 'none' }}
          onMouseEnter={e => (e.currentTarget.style.color = 'var(--ink)')}
          onMouseLeave={e => (e.currentTarget.style.color = 'var(--ink-mute)')}>
          cancel
        </Link>
      </div>
    </div>
  )
}
