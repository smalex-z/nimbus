import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import api from '@/api/client'
import { useAuth } from '@/context/AuthContext'
import { NimbusBrand, NimbusFooter } from '@/components/nimbus'

export default function Claim() {
  const { user, adminClaimed, refresh, refreshAdminStatus } = useAuth()
  const navigate = useNavigate()
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // If admin is already claimed, send to dashboard
  useEffect(() => {
    if (adminClaimed === true) {
      navigate('/', { replace: true })
    }
  }, [adminClaimed, navigate])

  const handleClaim = async () => {
    setError(null)
    setLoading(true)
    try {
      await api.post('/admin/claim')
      await Promise.all([refresh(), refreshAdminStatus()])
      navigate('/', { replace: true })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to claim admin')
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
        style={{ width: '100%', maxWidth: 480, position: 'relative', overflow: 'hidden' }}
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
          <span className="n-pill" style={{
            display: 'inline-flex', alignItems: 'center', gap: 6,
            padding: '4px 10px', borderRadius: 999,
            background: 'rgba(248,175,130,0.12)', border: '1px solid rgba(248,175,130,0.35)',
            fontSize: 11, fontWeight: 500, color: '#9a5c2e',
          }}>
            <span style={{
              width: 6, height: 6, borderRadius: '50%', background: '#F8AF82',
              display: 'inline-block',
            }} />
            first run
          </span>
        </div>

        {/* Body */}
        <div style={{ padding: '4px 32px 28px' }}>
          <h1
            className="n-display"
            style={{ fontSize: 34, lineHeight: 1.05, margin: '0 0 8px' }}
          >
            Claim{' '}
            <span className="n-display-italic">admin.</span>
          </h1>
          <p style={{ margin: '0 0 28px', fontSize: 14, color: 'var(--ink-body)', lineHeight: 1.6 }}>
            No admin exists yet. As the first user on this cluster,{' '}
            <strong style={{ color: 'var(--ink)', fontWeight: 600 }}>{user?.name}</strong>, you can
            claim the admin role. This can only be done once.
          </p>

          {error && (
            <div
              style={{
                marginBottom: 16,
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

          {/* Info box */}
          <div
            style={{
              padding: '14px 16px',
              borderRadius: 10,
              background: 'rgba(20,18,28,0.03)',
              border: '1px solid var(--hairline)',
              marginBottom: 20,
            }}
          >
            <p style={{ margin: '0 0 6px', fontSize: 12, fontWeight: 600, color: 'var(--ink)', letterSpacing: '0.04em', textTransform: 'uppercase' }}>
              What admin can do
            </p>
            <ul style={{ margin: 0, padding: '0 0 0 16px', fontSize: 13, color: 'var(--ink-mute)', lineHeight: 1.7 }}>
              <li>View all registered accounts</li>
              <li>Provision and manage VMs cluster-wide</li>
              <li>Access administrative settings</li>
            </ul>
          </div>

          <button
            className="n-btn n-btn-primary n-btn-block"
            onClick={handleClaim}
            disabled={loading}
          >
            {loading ? 'Claiming…' : 'Claim admin access'}
          </button>
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
