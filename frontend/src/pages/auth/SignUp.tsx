import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import api, { getProviders } from '@/api/client'
import type { OAuthProviders } from '@/api/client'
import { NimbusBrand, NimbusFooter, GithubIcon, GoogleIcon, ArrowRightIcon } from '@/components/nimbus'

const HEADING_PLAIN = 'Get '
const HEADING_ITALIC = 'started.'
const HEADING_FULL = HEADING_PLAIN + HEADING_ITALIC

function useTypingEffect(text: string, speed = 42) {
  const [displayed, setDisplayed] = useState('')
  const [done, setDone] = useState(false)

  useEffect(() => {
    let i = 0
    setDisplayed('')
    setDone(false)
    const id = setInterval(() => {
      i++
      setDisplayed(text.slice(0, i))
      if (i >= text.length) {
        setDone(true)
        clearInterval(id)
      }
    }, speed)
    return () => clearInterval(id)
  }, [text, speed])

  return { displayed, done }
}

export default function SignUp() {
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [done, setDone] = useState(false)
  const [providers, setProviders] = useState<OAuthProviders | null>(null)
  const { displayed, done: typingDone } = useTypingEffect(HEADING_FULL)

  useEffect(() => {
    getProviders().then(setProviders).catch(() => setProviders({ github: false, google: false }))
  }, [])

  const plainVisible = displayed.slice(0, HEADING_PLAIN.length)
  const italicVisible = displayed.slice(HEADING_PLAIN.length)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)

    if (password !== confirm) {
      setError('Passwords do not match')
      return
    }
    if (password.length < 8) {
      setError('Password must be at least 8 characters')
      return
    }

    try {
      setLoading(true)
      await api.post('/auth/register', { name, email, password })
      setDone(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '24px 16px',
      }}
    >
      <div
        className="glass"
        style={{
          width: '100%',
          maxWidth: 480,
          position: 'relative',
          overflow: 'hidden',
        }}
      >
        {/* Header */}
        <div
          style={{
            padding: '28px 32px 20px',
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
          }}
        >
          <NimbusBrand size="md" subtitle="vm provisioning" />
          <span className="n-pill n-pill-ok">
            <span className="n-pill-dot" />
            cluster online
          </span>
        </div>

        {/* Body */}
        <div style={{ padding: '4px 32px 28px' }}>
          <h1
            className="n-display"
            style={{ fontSize: 34, lineHeight: 1.05, margin: '0 0 8px' }}
          >
            {plainVisible}
            {italicVisible && <span className="n-display-italic">{italicVisible}</span>}
            <span
              className={typingDone ? 'n-cursor-blink' : ''}
              style={{ display: 'inline-block', width: 2, height: '0.8em', background: 'currentColor', marginLeft: 2, verticalAlign: 'middle' }}
            />
          </h1>
          <p
            style={{
              margin: '0 0 24px',
              fontSize: 14,
              color: 'var(--ink-body)',
              lineHeight: 1.5,
            }}
          >
            Create an account to start provisioning VMs.
          </p>

          {done ? (
            <div
              style={{
                padding: '20px',
                borderRadius: 12,
                background: 'rgba(31, 122, 77, 0.06)',
                border: '1px solid rgba(31, 122, 77, 0.18)',
                textAlign: 'center',
              }}
            >
              <p style={{ margin: '0 0 4px', fontSize: 15, fontWeight: 600, color: 'var(--ok)' }}>
                Account created!
              </p>
              <p style={{ margin: '0 0 16px', fontSize: 13, color: 'var(--ink-body)' }}>
                You can now sign in with your credentials.
              </p>
              <Link to="/login" className="n-btn n-btn-primary" style={{ display: 'inline-flex', gap: 6, textDecoration: 'none' }}>
                Go to sign in
              </Link>
            </div>
          ) : (
            <>
              {/* OAuth providers — only rendered when admin has configured them */}
              {(providers?.github || providers?.google) && (
                <>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 10, marginBottom: 20 }}>
                    {providers.github && (
                      <button className="n-provider" type="button" onClick={() => { window.location.href = '/api/auth/github' }}>
                        <GithubIcon size={18} />
                        <span style={{ flex: 1 }}>Continue with GitHub</span>
                        <ArrowRightIcon size={14} />
                      </button>
                    )}
                    {providers.google && (
                      <button className="n-provider" type="button" onClick={() => { window.location.href = '/api/auth/google' }}>
                        <GoogleIcon size={18} />
                        <span style={{ flex: 1 }}>Continue with Google</span>
                        <ArrowRightIcon size={14} />
                      </button>
                    )}
                  </div>
                  <div className="n-divider" style={{ marginBottom: 20 }}>or</div>
                </>
              )}

              {error && (
                <div
                  style={{
                    marginBottom: 14,
                    padding: '10px 14px',
                    borderRadius: 8,
                    background: 'rgba(184,58,58,0.06)',
                    border: '1px solid rgba(184,58,58,0.18)',
                    fontSize: 13,
                    color: 'var(--err)',
                  }}
                >
                  {error}
                </div>
              )}

              <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
                <div className="n-field">
                  <label className="n-label" htmlFor="signup-name">Full name</label>
                  <input
                    id="signup-name"
                    className="n-input"
                    type="text"
                    placeholder="Alex Zheng"
                    autoComplete="name"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    required
                  />
                </div>

                <div className="n-field">
                  <label className="n-label" htmlFor="signup-email">Email</label>
                  <input
                    id="signup-email"
                    className="n-input"
                    type="email"
                    placeholder="you@example.com"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    required
                  />
                </div>

                <div className="n-field">
                  <label className="n-label" htmlFor="signup-password">Password</label>
                  <input
                    id="signup-password"
                    className="n-input"
                    type="password"
                    placeholder="min. 8 characters"
                    autoComplete="new-password"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    required
                  />
                </div>

                <div className="n-field">
                  <label className="n-label" htmlFor="signup-confirm">Confirm password</label>
                  <input
                    id="signup-confirm"
                    className="n-input"
                    type="password"
                    placeholder="••••••••"
                    autoComplete="new-password"
                    value={confirm}
                    onChange={(e) => setConfirm(e.target.value)}
                    required
                  />
                </div>

                <button
                  className="n-btn n-btn-primary n-btn-block"
                  type="submit"
                  disabled={loading}
                  style={{ marginTop: 4 }}
                >
                  {loading ? 'Creating account…' : 'Create account'}
                </button>
              </form>

              <p
                style={{
                  marginTop: 20,
                  textAlign: 'center',
                  fontSize: 13,
                  color: 'var(--ink-mute)',
                }}
              >
                Already have an account?{' '}
                <Link to="/login" className="n-link" style={{ fontWeight: 500 }}>
                  Sign in
                </Link>
              </p>
            </>
          )}
        </div>

        <NimbusFooter
          left={`secured · ${window.location.host}`}
          right={
            <span style={{ display: 'inline-flex', gap: 12 }}>
              <a className="n-link">privacy</a>
              <a className="n-link">docs</a>
            </span>
          }
        />
      </div>
    </div>
  )
}
