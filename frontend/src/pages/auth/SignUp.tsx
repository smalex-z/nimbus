import { Link } from 'react-router-dom'
import { NimbusBlobs, NimbusBrand, NimbusFooter, GithubIcon, GoogleIcon, ArrowRightIcon } from '@/components/nimbus'

export default function SignUp() {
  return (
    <div
      style={{
        minHeight: '100vh',
        background: 'linear-gradient(180deg, var(--bg-top) 0%, var(--bg-bot) 100%)',
        position: 'relative',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '24px 16px',
      }}
    >
      <NimbusBlobs />

      <div
        className="n-card"
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
            Get{' '}
            <span className="n-display-italic">started.</span>
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

          {/* Sign-up form */}
          <form style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="n-field">
              <label className="n-label" htmlFor="signup-name">
                Full name
              </label>
              <input
                id="signup-name"
                className="n-input"
                type="text"
                placeholder="Alex Zheng"
                autoComplete="name"
              />
            </div>

            <div className="n-field">
              <label className="n-label" htmlFor="signup-email">
                Email
              </label>
              <input
                id="signup-email"
                className="n-input"
                type="email"
                placeholder="you@example.com"
                autoComplete="email"
              />
            </div>

            <div className="n-field">
              <label className="n-label" htmlFor="signup-password">
                Password
              </label>
              <input
                id="signup-password"
                className="n-input"
                type="password"
                placeholder="••••••••"
                autoComplete="new-password"
              />
            </div>

            <div className="n-field">
              <label className="n-label" htmlFor="signup-confirm">
                Confirm password
              </label>
              <input
                id="signup-confirm"
                className="n-input"
                type="password"
                placeholder="••••••••"
                autoComplete="new-password"
              />
            </div>

            <button
              className="n-btn n-btn-primary n-btn-block"
              type="submit"
              style={{ marginTop: 4 }}
            >
              Create account
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
