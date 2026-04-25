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
    <div className="space-y-6">
      <h1 className="text-2xl font-bold text-gray-900">Settings</h1>

      <Card title="Server Health">
        {error ? (
          <p className="text-sm text-red-600">{error}</p>
        ) : health ? (
          <dl className="grid grid-cols-2 gap-4">
            <div>
              <dt className="text-xs font-medium text-gray-500 uppercase">Status</dt>
              <dd className="mt-1 text-sm font-semibold text-green-600">{health.status}</dd>
            </div>
            <div>
              <dt className="text-xs font-medium text-gray-500 uppercase">Last Checked</dt>
              <dd className="mt-1 text-sm text-gray-900">
                {new Date(health.timestamp).toLocaleString()}
              </dd>
            </div>
          </dl>
        ) : (
          <p className="text-sm text-gray-500">Checking…</p>
        )}
      </Card>

      <Card title="About">
        <p className="text-sm text-gray-600">
          Homestack is a production-ready template for self-hosted applications.
          Customize this page to display your application settings.
        </p>
        <ul className="mt-4 space-y-1 text-sm text-gray-600 list-disc list-inside">
          <li>Backend: Go + Chi router</li>
          <li>Frontend: React 18 + TypeScript + Vite + Tailwind CSS</li>
          <li>Database: SQLite + GORM</li>
          <li>Deployment: Single binary with embedded frontend</li>
        </ul>
      </Card>
    </div>
  )
}
