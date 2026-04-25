import { useEffect, useState } from 'react'
import api from '@/api/client'
import type { User } from '@/types'
import Card from '@/components/ui/Card'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'

export default function Dashboard() {
  const [users, setUsers] = useState<User[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const fetchUsers = async () => {
    try {
      setLoading(true)
      const { data } = await api.get<User[]>('/users')
      setUsers(data ?? [])
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load users')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void fetchUsers()
  }, [])

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!name || !email) return
    try {
      setSubmitting(true)
      await api.post('/users', { name, email })
      setName('')
      setEmail('')
      await fetchUsers()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create user')
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async (id: number) => {
    try {
      await api.delete(`/users/${id}`)
      await fetchUsers()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete user')
    }
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
      <h1
        className="n-display"
        style={{ fontSize: 28, margin: 0 }}
      >
        Dashboard
      </h1>

      {error && (
        <div
          style={{
            padding: '12px 16px',
            borderRadius: 10,
            background: 'rgba(184,58,58,0.06)',
            border: '1px solid rgba(184,58,58,0.2)',
            color: 'var(--err)',
            fontSize: 13,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          {error}
          <button
            onClick={() => setError(null)}
            style={{
              background: 'none',
              border: 'none',
              color: 'var(--err)',
              cursor: 'pointer',
              fontSize: 12,
              textDecoration: 'underline',
            }}
          >
            Dismiss
          </button>
        </div>
      )}

      <Card title="Add User">
        <form onSubmit={handleCreate} style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
          <div style={{ flex: 1, minWidth: 140 }}>
            <Input
              placeholder="Name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>
          <div style={{ flex: 1, minWidth: 180 }}>
            <Input
              type="email"
              placeholder="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
            />
          </div>
          <Button type="submit" disabled={submitting}>
            {submitting ? 'Adding…' : 'Add'}
          </Button>
        </form>
      </Card>

      <Card title="Users">
        {loading ? (
          <p style={{ fontSize: 13, color: 'var(--ink-mute)', margin: 0 }}>Loading…</p>
        ) : users.length === 0 ? (
          <p style={{ fontSize: 13, color: 'var(--ink-mute)', margin: 0 }}>
            No users yet. Add one above!
          </p>
        ) : (
          <ul style={{ margin: 0, padding: 0, listStyle: 'none' }}>
            {users.map((user, i) => (
              <li
                key={user.id}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  padding: '12px 0',
                  borderTop: i === 0 ? 'none' : '1px solid var(--hairline)',
                }}
              >
                <div>
                  <p style={{ margin: 0, fontSize: 14, fontWeight: 500, color: 'var(--ink)' }}>
                    {user.name}
                  </p>
                  <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', fontFamily: 'var(--font-mono)', marginTop: 2 }}>
                    {user.email}
                  </p>
                </div>
                <Button variant="danger" onClick={() => handleDelete(user.id)}>
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}
