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
    <div className="space-y-6">
      <h1 className="text-2xl font-bold text-gray-900">Dashboard</h1>

      {error && (
        <div className="rounded-md bg-red-50 border border-red-200 p-4 text-sm text-red-700">
          {error}
          <button
            className="ml-2 underline"
            onClick={() => setError(null)}
          >
            Dismiss
          </button>
        </div>
      )}

      <Card title="Add User">
        <form onSubmit={handleCreate} className="flex flex-col sm:flex-row gap-3">
          <Input
            placeholder="Name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <Input
            type="email"
            placeholder="Email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
          <Button type="submit" disabled={submitting}>
            {submitting ? 'Adding…' : 'Add'}
          </Button>
        </form>
      </Card>

      <Card title="Users">
        {loading ? (
          <p className="text-sm text-gray-500">Loading…</p>
        ) : users.length === 0 ? (
          <p className="text-sm text-gray-500">No users yet. Add one above!</p>
        ) : (
          <ul className="divide-y divide-gray-100">
            {users.map((user) => (
              <li key={user.id} className="flex items-center justify-between py-3">
                <div>
                  <p className="text-sm font-medium text-gray-900">{user.name}</p>
                  <p className="text-xs text-gray-500">{user.email}</p>
                </div>
                <Button
                  variant="danger"
                  onClick={() => handleDelete(user.id)}
                >
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
