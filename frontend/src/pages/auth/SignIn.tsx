import { Link } from 'react-router-dom'
import { NimbusBrand, NimbusFooter, GithubIcon, GoogleIcon, ArrowRightIcon } from '@/components/nimbus'

export default function SignIn() {
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
          <p
            style={{
              margin: '0 0 24px',
              fontSize: 14,
              color: 'var(--ink-body)',
              lineHeight: 1.5,
            }}
          >
            Sign in to provision VMs on the cluster.
          </p>

          {/* OAuth providers */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10, marginBottom: 20 }}>
            <button className="n-provider" type="button">
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

          <div className="n-divider" style={{ marginBottom: 20 }}>
            or
          </div>

          {/* Email / password form */}
          <form style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="n-field">
              <label className="n-label" htmlFor="signin-email">
                Email
              </label>
              <input
                id="signin-email"
                className="n-input"
                type="email"
                placeholder="you@example.com"
                autoComplete="email"
              />
            </div>

            <div className="n-field">
              <label className="n-label" htmlFor="signin-password">
                Password
              </label>
              <input
                id="signin-password"
                className="n-input"
                type="password"
                placeholder="••••••••"
                autoComplete="current-password"
              />
            </div>

            <button
              className="n-btn n-btn-primary n-btn-block"
              type="submit"
              style={{ marginTop: 4 }}
            >
              Sign in
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
