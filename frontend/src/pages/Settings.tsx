import { useEffect, useState } from 'react'
import api from '@/api/client'
import type { HealthResponse } from '@/types'
import Card from '@/components/ui/Card'

export default function Settings() {
  const [health, setHealth] = useState<HealthResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api
      .get<HealthResponse>('/health')
      .then(({ data }) => setHealth(data))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : 'Failed to fetch health'),
      )
  }, [])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
      <h1 className="n-display" style={{ fontSize: 28, margin: 0 }}>
        Settings
      </h1>

      <Card title="Server Health">
        {error ? (
          <p style={{ fontSize: 13, color: 'var(--err)', margin: 0 }}>{error}</p>
        ) : health ? (
          <dl style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, margin: 0 }}>
            <div>
              <dt style={{ fontSize: 11, fontWeight: 500, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                Status
              </dt>
              <dd style={{ marginTop: 4, fontSize: 14, fontWeight: 600, color: 'var(--ok)', fontFamily: 'var(--font-mono)' }}>
                {health.status}
              </dd>
            </div>
            <div>
              <dt style={{ fontSize: 11, fontWeight: 500, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                Last checked
              </dt>
              <dd style={{ marginTop: 4, fontSize: 13, color: 'var(--ink)', fontFamily: 'var(--font-mono)' }}>
                {new Date(health.timestamp).toLocaleString()}
              </dd>
            </div>
          </dl>
        ) : (
          <p style={{ fontSize: 13, color: 'var(--ink-mute)', margin: 0 }}>Checking…</p>
        )}
      </Card>

      <Card title="About">
        <p style={{ fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.6, margin: '0 0 12px' }}>
          Nimbus is a self-hosted VM provisioning platform built on Proxmox VE.
        </p>
        <ul style={{ margin: 0, padding: '0 0 0 16px', display: 'flex', flexDirection: 'column', gap: 4 }}>
          {[
            'Backend: Go · Chi router',
            'Frontend: React 18 · TypeScript · Vite · Tailwind',
            'Database: SQLite · GORM',
            'Deployment: Single binary · systemd',
          ].map((item) => (
            <li key={item} style={{ fontSize: 13, color: 'var(--ink-body)', fontFamily: 'var(--font-mono)' }}>
              {item}
            </li>
          ))}
        </ul>
      </Card>
    </div>
  )
}
