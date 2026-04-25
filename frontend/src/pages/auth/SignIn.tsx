import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import api from '@/api/client'
import { NimbusBrand, NimbusFooter, GithubIcon, GoogleIcon, ArrowRightIcon } from '@/components/nimbus'

export default function SignIn() {
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    try {
      setLoading(true)
      await api.post('/auth/login', { email, password })
      navigate('/')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong')
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
            Welcome{' '}
            <span className="n-display-italic">back.</span>
          </h1>
          <p style={{ margin: '0 0 24px', fontSize: 14, color: 'var(--ink-body)', lineHeight: 1.5 }}>
            Sign in to provision VMs on the cluster.
          </p>

          {/* OAuth providers */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10, marginBottom: 20 }}>
            <button className="n-provider" type="button" onClick={() => { window.location.href = '/api/auth/github' }}>
              <GithubIcon size={18} />
              <span style={{ flex: 1 }}>Continue with GitHub</span>
              <ArrowRightIcon size={14} />
            </button>
            <button className="n-provider" type="button">
              <GoogleIcon size={18} />
              <span style={{ flex: 1 }}>Continue with Google</span>
              <ArrowRightIcon size={14} />
            </button>
          </div>

          <div className="n-divider" style={{ marginBottom: 20 }}>or</div>

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
              <label className="n-label" htmlFor="signin-email">Email</label>
              <input
                id="signin-email"
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
              <label className="n-label" htmlFor="signin-password">Password</label>
              <input
                id="signin-password"
                className="n-input"
                type="password"
                placeholder="••••••••"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
            </div>

            <button
              className="n-btn n-btn-primary n-btn-block"
              type="submit"
              disabled={loading}
              style={{ marginTop: 4 }}
            >
              {loading ? 'Signing in…' : 'Sign in'}
            </button>
          </form>

          <p style={{ marginTop: 20, textAlign: 'center', fontSize: 13, color: 'var(--ink-mute)' }}>
            Don&apos;t have an account?{' '}
            <Link to="/signup" className="n-link" style={{ fontWeight: 500 }}>
              Create one
            </Link>
          </p>
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
