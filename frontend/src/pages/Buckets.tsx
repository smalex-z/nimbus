import { useEffect, useMemo, useState } from 'react'
import {
  createBucket,
  deleteBucket,
  getBucketCredentials,
  isStorageNotDeployed,
  isStorageNotReady,
  listBuckets,
} from '@/api/client'
import type { BucketCredentials, UserBucket } from '@/api/client'
import DeleteBucketConfirm from '@/components/ui/DeleteBucketConfirm'

// Three states the page can render:
//  - "no_storage": admin hasn't deployed object storage yet → empty-state card
//  - "not_ready": storage exists but is mid-deploy → small banner
//  - "ready": list + create form + credentials button
//
// We compose the storage state from listBuckets()'s thrown error string —
// the backend returns 503 with a stable message we pattern-match.

type StorageState = 'loading' | 'no_storage' | 'not_ready' | 'ready' | 'error'

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}

function EmptyStorageCard({ message }: { message: string }) {
  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 12 }}
    >
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
        Object storage not available
      </span>
      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        {message}
      </p>
    </div>
  )
}

function CredentialsModal({
  creds,
  buckets,
  onClose,
}: {
  creds: BucketCredentials
  buckets: UserBucket[]
  onClose: () => void
}) {
  const [revealed, setRevealed] = useState(false)
  const [tab, setTab] = useState<'env' | 'python' | 'js' | 'cli'>('env')

  // Use a real bucket name if the user has one — snippets are then
  // copy-pasteable verbatim. Otherwise insert an obvious placeholder
  // (`<your-bucket-name>`) so the user knows exactly what to swap.
  const hasRealBucket = buckets.length > 0
  const bucketName = hasRealBucket ? buckets[0].name : `${creds.prefix}-<your-bucket-name>`

  const snippets = useMemo(() => {
    return {
      env: `S3_ENDPOINT=${creds.endpoint}
S3_ACCESS_KEY=${creds.access_key}
S3_SECRET_KEY=${creds.secret_key}
S3_BUCKET=${bucketName}`,
      python: `import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="${creds.endpoint}",
    aws_access_key_id="${creds.access_key}",
    aws_secret_access_key="${creds.secret_key}",
)
s3.put_object(Bucket="${bucketName}", Key="hello.txt", Body=b"hi")`,
      js: `import { S3Client, PutObjectCommand } from "@aws-sdk/client-s3"

const s3 = new S3Client({
  endpoint: "${creds.endpoint}",
  region: "us-east-1",
  credentials: {
    accessKeyId: "${creds.access_key}",
    secretAccessKey: "${creds.secret_key}",
  },
  forcePathStyle: true,
})
await s3.send(new PutObjectCommand({
  Bucket: "${bucketName}",
  Key: "hello.txt",
  Body: "hi",
}))`,
      cli: `aws --endpoint-url ${creds.endpoint} s3 ls s3://${bucketName}/
aws --endpoint-url ${creds.endpoint} s3 cp ./file.txt s3://${bucketName}/file.txt`,
    }
  }, [creds, bucketName])

  const copy = (text: string) => {
    void navigator.clipboard.writeText(text)
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(0,0,0,0.4)',
        display: 'grid',
        placeItems: 'center',
        zIndex: 100,
      }}
      onClick={onClose}
    >
      <div
        className="glass"
        onClick={(e) => e.stopPropagation()}
        style={{
          padding: '24px 28px',
          display: 'flex',
          flexDirection: 'column',
          gap: 16,
          width: 'min(640px, 92vw)',
          maxHeight: '88vh',
          overflowY: 'auto',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <span style={{ fontSize: 17, fontWeight: 600 }}>S3 credentials</span>
          <button type="button" className="n-btn n-btn-ghost" onClick={onClose}>
            Close
          </button>
        </div>

        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Use these in any S3-compatible SDK from a webapp running on a
          Nimbus VM. The MinIO host is on the internal cluster network only —
          off-cluster apps and browsers can't reach it.
        </p>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <Field label="Endpoint" value={creds.endpoint} onCopy={() => copy(creds.endpoint)} />
          <Field label="Access key" value={creds.access_key} onCopy={() => copy(creds.access_key)} />
          <Field
            label="Secret key"
            value={revealed ? creds.secret_key : '•'.repeat(32)}
            onCopy={() => copy(creds.secret_key)}
            actions={
              <button
                type="button"
                className="n-btn n-btn-ghost"
                onClick={() => setRevealed((v) => !v)}
                style={{ padding: '2px 10px', fontSize: 12 }}
              >
                {revealed ? 'Hide' : 'Reveal'}
              </button>
            }
          />
          <Field
            label="Name prefix"
            value={creds.prefix}
            help={`Buckets you create are named ${creds.prefix}-<your-name>`}
            onCopy={() => copy(creds.prefix)}
          />
        </div>

        <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
          {hasRealBucket ? (
            <>
              Snippets below use <code style={{ fontFamily: 'var(--font-mono, monospace)' }}>{bucketName}</code>.
              Swap it for any other bucket you own (suffix only — the prefix
              part <code style={{ fontFamily: 'var(--font-mono, monospace)' }}>{creds.prefix}-</code> is fixed).
            </>
          ) : (
            <>
              You don't have any buckets yet — the snippets show
              {' '}<code style={{ fontFamily: 'var(--font-mono, monospace)' }}>{bucketName}</code> as a placeholder.
              Replace <code style={{ fontFamily: 'var(--font-mono, monospace)' }}>{'<your-bucket-name>'}</code> with the suffix you'll
              type when you create one (e.g. <code style={{ fontFamily: 'var(--font-mono, monospace)' }}>{creds.prefix}-uploads</code>).
            </>
          )}
        </p>

        <div style={{ display: 'flex', gap: 4, borderBottom: '1px solid var(--line)' }}>
          {(['env', 'python', 'js', 'cli'] as const).map((t) => (
            <button
              key={t}
              type="button"
              onClick={() => setTab(t)}
              style={{
                padding: '6px 12px',
                fontSize: 12,
                fontWeight: tab === t ? 600 : 400,
                color: tab === t ? 'var(--ink)' : 'var(--ink-mute)',
                background: 'transparent',
                border: 'none',
                borderBottom: tab === t ? '2px solid var(--ink)' : '2px solid transparent',
                cursor: 'pointer',
              }}
            >
              {t === 'env' ? '.env' : t === 'js' ? 'JS / TS' : t === 'cli' ? 'aws-cli' : 'Python'}
            </button>
          ))}
        </div>

        <div style={{ position: 'relative' }}>
          <pre
            style={{
              background: 'rgba(20,18,28,0.04)',
              border: '1px solid var(--line)',
              borderRadius: 8,
              padding: 12,
              fontSize: 12,
              fontFamily: 'var(--font-mono, ui-monospace, SFMono-Regular, monospace)',
              overflowX: 'auto',
              margin: 0,
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-all',
            }}
          >
            {snippets[tab]}
          </pre>
          <button
            type="button"
            className="n-btn n-btn-ghost"
            onClick={() => copy(snippets[tab])}
            style={{ position: 'absolute', top: 6, right: 6, padding: '2px 10px', fontSize: 12 }}
          >
            Copy
          </button>
        </div>
      </div>
    </div>
  )
}

function Field({
  label,
  value,
  onCopy,
  actions,
  help,
}: {
  label: string
  value: string
  onCopy: () => void
  actions?: React.ReactNode
  help?: string
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 }}>
        <span style={{ color: 'var(--ink-mute)', fontSize: 12 }}>{label}</span>
        <div style={{ display: 'flex', gap: 4 }}>
          {actions}
          <button
            type="button"
            className="n-btn n-btn-ghost"
            onClick={onCopy}
            style={{ padding: '2px 10px', fontSize: 12 }}
          >
            Copy
          </button>
        </div>
      </div>
      <span
        style={{
          fontFamily: 'var(--font-mono, ui-monospace, SFMono-Regular, monospace)',
          fontSize: 12,
          color: 'var(--ink)',
          wordBreak: 'break-all',
          padding: '6px 10px',
          background: 'rgba(20,18,28,0.03)',
          borderRadius: 6,
          border: '1px solid var(--line)',
        }}
      >
        {value}
      </span>
      {help && <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>{help}</span>}
    </div>
  )
}

function NewBucketForm({
  prefix,
  onCreated,
  disabled,
}: {
  prefix: string | null
  onCreated: (b: UserBucket) => void
  disabled: boolean
}) {
  const [namePart, setNamePart] = useState('')
  const [creating, setCreating] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const composedName = prefix && namePart ? `${prefix}-${namePart}` : ''

  const handle = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr(null)
    const v = namePart.trim().toLowerCase()
    if (!v) return
    setCreating(true)
    try {
      const created = await createBucket(v)
      setNamePart('')
      onCreated(created)
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Create failed')
    } finally {
      setCreating(false)
    }
  }

  return (
    <form
      onSubmit={handle}
      style={{ display: 'flex', flexDirection: 'column', gap: 8 }}
    >
      <div style={{ display: 'flex', gap: 8, alignItems: 'stretch' }}>
        <input
          className="n-input"
          type="text"
          placeholder="my-app-uploads"
          value={namePart}
          onChange={(e) => setNamePart(e.target.value)}
          disabled={creating || disabled}
          style={{ flex: 1 }}
        />
        <button
          type="submit"
          className="n-btn n-btn-primary"
          disabled={creating || disabled || !namePart.trim()}
        >
          {creating ? 'Creating…' : 'Create bucket'}
        </button>
      </div>
      <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>
        {composedName
          ? `Will be created as ${composedName}`
          : 'Lowercase letters, digits, hyphens. 3-30 chars.'}
      </span>
      {err && <span style={{ fontSize: 13, color: 'var(--err)' }}>{err}</span>}
    </form>
  )
}

export default function Buckets() {
  const [state, setState] = useState<StorageState>('loading')
  const [storageMsg, setStorageMsg] = useState<string>('')
  const [buckets, setBuckets] = useState<UserBucket[]>([])
  const [creds, setCreds] = useState<BucketCredentials | null>(null)
  const [showCreds, setShowCreds] = useState(false)
  const [credsErr, setCredsErr] = useState<string | null>(null)
  const [listErr, setListErr] = useState<string | null>(null)
  const [confirmingDelete, setConfirmingDelete] = useState<UserBucket | null>(null)

  const refresh = async () => {
    setListErr(null)
    try {
      const list = await listBuckets()
      setBuckets(list)
      setState('ready')
    } catch (err) {
      if (isStorageNotDeployed(err)) {
        setState('no_storage')
        setStorageMsg(
          "Object storage isn't enabled on this cluster yet — ask an admin to deploy it from the S3 storage page.",
        )
      } else if (isStorageNotReady(err)) {
        setState('not_ready')
        setStorageMsg(
          "Object storage is being set up. This usually takes 3-5 minutes; refresh shortly.",
        )
      } else {
        setState('error')
        setListErr(err instanceof Error ? err.message : 'Failed to load buckets')
      }
    }
  }

  useEffect(() => {
    refresh()
  }, [])

  // Lazy-fetch credentials only when the user opens the modal (or first
  // creates a bucket — we want the prefix in NewBucketForm). For the
  // prefix-preview we fetch on mount once the storage is known ready.
  useEffect(() => {
    if (state !== 'ready') return
    if (creds) return
    getBucketCredentials()
      .then(setCreds)
      .catch((err) => setCredsErr(err instanceof Error ? err.message : 'Failed to load credentials'))
  }, [state, creds])

  const requestDelete = (b: UserBucket) => setConfirmingDelete(b)

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Buckets
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Per-user S3 buckets on the cluster's shared MinIO host. Each user
          has a stable name prefix and a service account scoped to their own
          buckets — others can't see or read them.
        </p>
      </div>

      {state === 'loading' && (
        <div className="glass" style={{ padding: '24px 28px' }}>
          <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
        </div>
      )}

      {(state === 'no_storage' || state === 'not_ready') && (
        <EmptyStorageCard message={storageMsg} />
      )}

      {state === 'error' && (
        <div className="glass" style={{ padding: '24px 28px' }}>
          <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{listErr}</p>
        </div>
      )}

      {state === 'ready' && (
        <>
          <div
            className="glass"
            style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 16 }}
          >
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <span style={{ fontSize: 15, fontWeight: 600 }}>
                {buckets.length} {buckets.length === 1 ? 'bucket' : 'buckets'}
              </span>
              <button
                type="button"
                className="n-btn n-btn-secondary"
                onClick={() => {
                  if (creds) setShowCreds(true)
                  else if (credsErr) alert(credsErr)
                }}
                disabled={!creds}
              >
                Show credentials
              </button>
            </div>

            <NewBucketForm
              prefix={creds?.prefix ?? null}
              disabled={!creds}
              onCreated={(b) => setBuckets((prev) => [b, ...prev])}
            />

            {credsErr && !creds && (
              <span style={{ fontSize: 13, color: 'var(--err)' }}>{credsErr}</span>
            )}
          </div>

          {buckets.length === 0 ? (
            <div className="glass" style={{ padding: '24px 28px' }}>
              <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
                No buckets yet. Create one above.
              </p>
            </div>
          ) : (
            <div className="glass" style={{ padding: 0, overflow: 'hidden' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
                <thead>
                  <tr style={{ textAlign: 'left', color: 'var(--ink-mute)', borderBottom: '1px solid var(--line)' }}>
                    <th style={{ padding: '12px 20px', fontWeight: 500 }}>Name</th>
                    <th style={{ padding: '12px 20px', fontWeight: 500 }}>Objects</th>
                    <th style={{ padding: '12px 20px', fontWeight: 500 }}>Size</th>
                    <th style={{ padding: '12px 20px', fontWeight: 500 }}>Created</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {buckets.map((b) => (
                    <tr key={b.name} style={{ borderTop: '1px solid var(--line)' }}>
                      <td
                        style={{
                          padding: '10px 20px',
                          color: 'var(--ink)',
                          fontFamily: 'var(--font-mono, ui-monospace, SFMono-Regular, monospace)',
                        }}
                      >
                        {b.name}
                      </td>
                      <td style={{ padding: '10px 20px', color: 'var(--ink-body)' }}>{b.object_count}</td>
                      <td style={{ padding: '10px 20px', color: 'var(--ink-body)' }}>
                        {formatBytes(b.total_size_bytes)}
                      </td>
                      <td style={{ padding: '10px 20px', color: 'var(--ink-mute)' }}>
                        {new Date(b.created_at).toLocaleDateString()}
                      </td>
                      <td style={{ padding: '10px 20px', textAlign: 'right' }}>
                        <button
                          type="button"
                          className="n-btn n-btn-ghost"
                          onClick={() => requestDelete(b)}
                          style={{ color: 'var(--err)' }}
                        >
                          Delete
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      {showCreds && creds && (
        <CredentialsModal
          creds={creds}
          buckets={buckets}
          onClose={() => setShowCreds(false)}
        />
      )}

      {confirmingDelete && (
        <DeleteBucketConfirm
          bucket={{
            name: confirmingDelete.name,
            objectCount: confirmingDelete.object_count,
            totalSizeBytes: confirmingDelete.total_size_bytes,
            createdAt: confirmingDelete.created_at,
          }}
          onConfirm={() => deleteBucket(confirmingDelete.name)}
          onDeleted={() => {
            const name = confirmingDelete.name
            setConfirmingDelete(null)
            setBuckets((prev) => prev.filter((b) => b.name !== name))
          }}
          onCancel={() => setConfirmingDelete(null)}
        />
      )}
    </div>
  )
}
