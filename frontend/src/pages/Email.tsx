import { useEffect, useState } from 'react'
import { getSMTPSettings, saveSMTPSettings } from '@/api/client'
import type { SMTPSettingsView, SaveSMTPRequest } from '@/api/client'

// Email — admin-only SMTP configuration. Today the saved config is
// dormant: nothing reads it for sending yet. The /users page's
// "Email N unlinked users" button gates on this card's Configured +
// Enabled state, but the actual recovery-email send pipeline lands in
// a follow-up release. Storing now (vs. later) means the schema and
// form scaffolding are ready when send wires up.
//
// Password handling follows the standard "edit secrets" pattern:
// leave the field blank to keep the existing stored value (the
// placeholder shows ••••••••), or type a new one to replace it. The
// request omits `password` when blank, sends the new value otherwise.
// Backend encrypts at rest with the same secrets.Cipher used for the
// SSH key vault.
export default function Email() {
  const [view, setView] = useState<SMTPSettingsView | null>(null)
  const [host, setHost] = useState('')
  const [port, setPort] = useState(587)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [fromAddress, setFromAddress] = useState('')
  const [encryption, setEncryption] = useState<'starttls' | 'tls' | 'none'>('starttls')
  const [enabled, setEnabled] = useState(false)
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    getSMTPSettings()
      .then((v) => {
        setView(v)
        setHost(v.host)
        setPort(v.port || 587)
        setUsername(v.username)
        setFromAddress(v.from_address)
        setEncryption((v.encryption as 'starttls' | 'tls' | 'none') || 'starttls')
        setEnabled(v.enabled)
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed to load'))
  }, [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    setBusy(true)
    try {
      const req: SaveSMTPRequest = {
        host: host.trim(),
        port,
        username: username.trim(),
        from_address: fromAddress.trim(),
        encryption,
        enabled,
      }
      if (password) req.password = password
      const next = await saveSMTPSettings(req)
      setView(next)
      setPassword('')
      setSaved(true)
      setTimeout(() => setSaved(false), 2500)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px', display: 'inline-flex', alignItems: 'center', gap: 10 }}>
          Email
          <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-0.5 rounded">
            Preview
          </span>
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Outbound SMTP for account-recovery emails. Configure once here;
          the Users page can then trigger magic-link emails to password-only
          users. The send pipeline itself ships in a follow-up release —
          credentials saved here stay dormant until then.
        </p>
      </div>

      {view === null && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      )}

      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}

      {view && (
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
          <div className="lg:col-span-2">
            <form onSubmit={submit} className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
                <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
                  SMTP server
                </span>
                {view.configured && view.enabled ? (
                  <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
                    <span className="n-pill-dot" />
                    enabled
                  </span>
                ) : view.configured ? (
                  <span
                    className="n-pill"
                    style={{
                      fontSize: 10,
                      color: 'var(--warn)',
                      background: 'rgba(184,101,15,0.10)',
                      border: '1px solid rgba(184,101,15,0.25)',
                    }}
                  >
                    configured · disabled
                  </span>
                ) : (
                  <span
                    className="n-pill"
                    style={{
                      fontSize: 10,
                      color: 'var(--ink-mute)',
                      background: 'rgba(20,18,28,0.04)',
                      border: '1px solid var(--line)',
                    }}
                  >
                    not configured
                  </span>
                )}
              </div>

              <div className="n-field">
                <label className="n-label" htmlFor="smtp-host">Host</label>
                <input
                  id="smtp-host"
                  className="n-input"
                  type="text"
                  placeholder="smtp.example.com"
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  required
                />
              </div>

              <div style={{ display: 'flex', gap: 12 }}>
                <div className="n-field" style={{ flex: '0 0 120px' }}>
                  <label className="n-label" htmlFor="smtp-port">Port</label>
                  <input
                    id="smtp-port"
                    className="n-input"
                    type="number"
                    min={1}
                    max={65535}
                    value={port}
                    onChange={(e) => setPort(Number(e.target.value) || 587)}
                  />
                </div>
                <div className="n-field" style={{ flex: 1 }}>
                  <label className="n-label" htmlFor="smtp-encryption">Encryption</label>
                  <select
                    id="smtp-encryption"
                    className="n-input"
                    value={encryption}
                    onChange={(e) => setEncryption(e.target.value as 'starttls' | 'tls' | 'none')}
                  >
                    <option value="starttls">STARTTLS (port 587)</option>
                    <option value="tls">TLS (port 465)</option>
                    <option value="none">None — plain SMTP</option>
                  </select>
                </div>
              </div>

              <div className="n-field">
                <label className="n-label" htmlFor="smtp-username">Username</label>
                <input
                  id="smtp-username"
                  className="n-input"
                  type="text"
                  placeholder="apikey, postmaster@…, etc."
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                />
              </div>

              <div className="n-field">
                <label className="n-label" htmlFor="smtp-password">
                  Password{view.has_password ? <span style={{ marginLeft: 8, fontSize: 11, color: 'var(--ink-mute)' }}>· stored — leave blank to keep</span> : null}
                </label>
                <input
                  id="smtp-password"
                  className="n-input"
                  type="password"
                  placeholder={view.has_password ? '••••••••' : ''}
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="new-password"
                />
              </div>

              <div className="n-field">
                <label className="n-label" htmlFor="smtp-from">From address</label>
                <input
                  id="smtp-from"
                  className="n-input"
                  type="email"
                  placeholder="nimbus@example.com"
                  value={fromAddress}
                  onChange={(e) => setFromAddress(e.target.value)}
                  required
                />
              </div>

              <label
                style={{
                  display: 'flex',
                  alignItems: 'flex-start',
                  gap: 10,
                  padding: '10px 12px',
                  border: '1px solid var(--line)',
                  borderRadius: 10,
                  cursor: 'pointer',
                  background: enabled ? 'rgba(20,18,28,0.05)' : 'rgba(20,18,28,0.02)',
                }}
              >
                <input
                  type="checkbox"
                  checked={enabled}
                  onChange={(e) => setEnabled(e.target.checked)}
                  style={{ marginTop: 3 }}
                />
                <span>
                  <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
                    Enable outbound mail
                  </span>
                  <span style={{ display: 'block', fontSize: 12, color: 'var(--ink-body)', lineHeight: 1.5, marginTop: 2 }}>
                    Off by default. Flip on once you've tested credentials in
                    your provider's web UI — turning this on is the gate that
                    unlocks the email-stragglers button on the Users page.
                  </span>
                </span>
              </label>

              <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginTop: 4 }}>
                <button
                  type="submit"
                  className="n-btn n-btn-primary"
                  disabled={busy}
                  style={{ minWidth: 100 }}
                >
                  {busy ? 'Saving…' : 'Save'}
                </button>
                {saved && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
              </div>
            </form>
          </div>

          <div className="lg:col-span-1">
            <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 12 }}>
              <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
                What this powers
              </span>
              <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
                Once SMTP is enabled and the send pipeline ships, admins on
                /users can email password-only accounts a magic-link
                recovery URL. Clicking the link signs the user in (and
                unsuspends them if needed) and drops them on /account so
                they can connect Google or GitHub.
              </p>
              <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
                Until then the button stays disabled. Saving credentials
                here is harmless — the server stores them encrypted but
                doesn't dial out yet.
              </p>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
