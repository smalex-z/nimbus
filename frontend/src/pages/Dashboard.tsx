import { useEffect, useState } from 'react'
import api from '@/api/client'
import { useAuth } from '@/context/AuthContext'
import type { User } from '@/types'

function AccountRow({ user, isFirst }: { user: User; isFirst: boolean }) {
  return (
    <li
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '14px 0',
        borderTop: isFirst ? 'none' : '1px solid var(--hairline)',
      }}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <span style={{ fontSize: 14, fontWeight: 500, color: 'var(--ink)' }}>
          {user.name}
        </span>
        <span style={{ fontSize: 12, color: 'var(--ink-mute)', fontFamily: 'var(--font-mono)' }}>
          {user.email}
        </span>
      </div>
      {user.is_admin && (
        <span style={{
          fontSize: 10,
          fontWeight: 600,
          letterSpacing: '0.05em',
          textTransform: 'uppercase',
          fontFamily: 'var(--font-sans)',
          color: '#9a5c2e',
          background: 'rgba(248,175,130,0.15)',
          border: '1px solid rgba(248,175,130,0.4)',
          padding: '3px 8px',
          borderRadius: 4,
        }}>
          admin
        </span>
      )}
    </li>
  )
}

function AccountsCard({ title, users, loading, error }: {
  title: string
  users: User[]
  loading: boolean
  error: string | null
}) {
  return (
    <div
      className="glass"
      style={{ padding: '20px 24px' }}
    >
      <p style={{
        margin: '0 0 16px',
        fontSize: 11,
        fontWeight: 600,
        letterSpacing: '0.07em',
        textTransform: 'uppercase',
        color: 'var(--ink-mute)',
        fontFamily: 'var(--font-mono)',
      }}>
        {title}
      </p>

      {loading ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      ) : error ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      ) : users.length === 0 ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>No accounts found.</p>
      ) : (
        <ul style={{ margin: 0, padding: 0, listStyle: 'none' }}>
          {users.map((u, i) => (
            <AccountRow key={u.id} user={u} isFirst={i === 0} />
          ))}
        </ul>
      )}
    </div>
  )
}

export default function Dashboard() {
  const { user } = useAuth()
  const [accounts, setAccounts] = useState<User[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api.get<User[]>('/users')
      .then(({ data }) => setAccounts(data ?? []))
      .catch((err) => setError(err instanceof Error ? err.message : 'Failed to load accounts'))
      .finally(() => setLoading(false))
  }, [])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 24 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 4px' }}>
          Dashboard
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-mute)' }}>
          {user?.is_admin
            ? 'Admin view — all registered accounts on this cluster.'
            : 'Your account details on this cluster.'}
        </p>
      </div>

      <AccountsCard
        title={user?.is_admin ? `Accounts · ${loading ? '…' : accounts.length}` : 'Your account'}
        users={accounts}
        loading={loading}
        error={error}
      />
    </div>
  )
}
