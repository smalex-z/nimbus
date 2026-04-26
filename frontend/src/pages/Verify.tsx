import { useEffect, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import nimbusLogo from '@/assets/Nimbus_Logo.png'
import api, { verifyAccessCode } from '@/api/client'
import { useAuth } from '@/hooks/useAuth'

const CODE_LENGTH = 8

export default function Verify() {
  const { refresh } = useAuth()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const stale = params.get('stale') === '1'
  const [digits, setDigits] = useState<string[]>(Array(CODE_LENGTH).fill(''))
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const inputs = useRef<Array<HTMLInputElement | null>>([])

  useEffect(() => {
    inputs.current[0]?.focus()
  }, [])

  const handleChange = (idx: number, value: string) => {
    const sanitized = value.replace(/\D/g, '')
    if (sanitized.length > 1) {
      // Pasted multi-char value — fan out across boxes starting at idx.
      const next = [...digits]
      for (let i = 0; i < sanitized.length && idx + i < CODE_LENGTH; i++) {
        next[idx + i] = sanitized[i]
      }
      setDigits(next)
      const lastFilled = Math.min(idx + sanitized.length, CODE_LENGTH) - 1
      inputs.current[Math.min(lastFilled + 1, CODE_LENGTH - 1)]?.focus()
      return
    }
    const next = [...digits]
    next[idx] = sanitized
    setDigits(next)
    if (sanitized && idx < CODE_LENGTH - 1) {
      inputs.current[idx + 1]?.focus()
    }
  }

  const handleKeyDown = (idx: number, e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Backspace' && !digits[idx] && idx > 0) {
      inputs.current[idx - 1]?.focus()
    }
    if (e.key === 'ArrowLeft' && idx > 0) inputs.current[idx - 1]?.focus()
    if (e.key === 'ArrowRight' && idx < CODE_LENGTH - 1) inputs.current[idx + 1]?.focus()
  }

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (submitting) return
    setError(null)
    const code = digits.join('')
    if (code.length !== CODE_LENGTH) {
      setError(`Please enter all ${CODE_LENGTH} digits`)
      return
    }
    try {
      setSubmitting(true)
      await verifyAccessCode(code)
      await refresh()
      let resume = '/'
      try {
        const stored = sessionStorage.getItem('nimbus_resume_path')
        if (stored && stored !== '/verify') resume = stored
        sessionStorage.removeItem('nimbus_resume_path')
      } catch {
        // ignore
      }
      navigate(resume, { replace: true })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Verification failed')
      setDigits(Array(CODE_LENGTH).fill(''))
      inputs.current[0]?.focus()
    } finally {
      setSubmitting(false)
    }
  }

  const handleSignOut = async () => {
    try {
      await api.post('/auth/logout')
    } finally {
      window.location.replace('/login')
    }
  }

  const allFilled = digits.every((d) => d !== '')

  return (
    <div className="min-h-screen flex flex-col">
      <nav
        className="sticky top-0 z-50 border-b border-line"
        style={{
          backdropFilter: 'blur(20px) saturate(140%)',
          WebkitBackdropFilter: 'blur(20px) saturate(140%)',
          background: 'rgba(255,255,255,0.75)',
        }}
      >
        <div className="max-w-[1200px] mx-auto px-8 py-5 flex items-center justify-between">
          <div className="flex items-center">
            <img src={nimbusLogo} alt="Nimbus" className="h-8 w-auto" />
          </div>
          <div className="flex gap-1 items-center">
            <span className="px-3.5 py-2 rounded-[8px] text-sm font-medium bg-[rgba(27,23,38,0.08)] text-ink">
              Verify
            </span>
            <div className="w-px h-4 bg-[rgba(20,18,28,0.1)] mx-1.5" />
            <button
              onClick={handleSignOut}
              className="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors"
            >
              Sign out
            </button>
          </div>
        </div>
      </nav>

      <main className="flex-1 flex items-center justify-center px-6">
        <div
          className="glass"
          style={{ width: '100%', maxWidth: 520, padding: '36px 36px 32px' }}
        >
          <div className="eyebrow">Access required</div>
          <h1 className="n-display" style={{ fontSize: 30, margin: '4px 0 10px' }}>
            Enter your <span className="n-display-italic">access code</span>
          </h1>
          <p style={{ margin: '0 0 22px', fontSize: 14, color: 'var(--ink-body)', lineHeight: 1.5 }}>
            Your administrator issued an 8-digit access code that unlocks the
            console. Type it below to continue.
          </p>

          {stale && (
            <div
              style={{
                marginBottom: 18,
                padding: '12px 14px',
                borderRadius: 10,
                background: 'rgba(248,175,130,0.12)',
                border: '1px solid rgba(248,175,130,0.4)',
                fontSize: 13,
                color: '#9a5c2e',
                lineHeight: 1.5,
              }}
            >
              The access code has been changed. Please contact your
              administrator for the new code.
            </div>
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

          <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 18 }}>
            <div
              style={{
                display: 'grid',
                gridTemplateColumns: `repeat(${CODE_LENGTH}, 1fr)`,
                gap: 8,
              }}
            >
              {digits.map((d, idx) => (
                <input
                  key={idx}
                  ref={(el) => { inputs.current[idx] = el }}
                  className="n-input"
                  inputMode="numeric"
                  pattern="[0-9]*"
                  maxLength={CODE_LENGTH}
                  value={d}
                  onChange={(e) => handleChange(idx, e.target.value)}
                  onKeyDown={(e) => handleKeyDown(idx, e)}
                  style={{
                    textAlign: 'center',
                    fontFamily: 'Geist Mono, monospace',
                    fontSize: 22,
                    padding: '14px 0',
                  }}
                />
              ))}
            </div>

            <button
              type="submit"
              className="n-btn n-btn-primary n-btn-block"
              disabled={submitting || !allFilled}
            >
              {submitting ? 'Verifying…' : 'Verify'}
            </button>
          </form>
        </div>
      </main>
    </div>
  )
}
