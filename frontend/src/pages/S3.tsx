import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  deleteS3Storage,
  deployS3Storage,
  getS3Storage,
} from '@/api/client'
import type { S3DeployProgress, S3StorageView } from '@/api/client'
import DeleteS3Confirm from '@/components/ui/DeleteS3Confirm'

// Phase-3 admin page. The page has two states:
//  1. No storage yet → render <DeployPanel/>: disk-size slider + button.
//  2. Storage exists → render <StatusPanel/> + <BucketsPanel/>.
//
// The deploy POST is NDJSON-streamed; the panel surfaces each progress
// event as a checklist line so the admin sees forward motion (clone,
// configure, start, agent wait, then a "bootstrap MinIO" line we emit
// client-side once Provision returns).

const PROGRESS_LABELS: Record<string, string> = {
  reserve_ip: 'Reserved IP from pool',
  clone_template: 'Cloned VM template',
  configure_vm: 'Configured cloud-init',
  start_vm: 'Started VM',
  wait_guest_agent: 'Guest agent reachable',
}

function StatusPill({ status }: { status: S3StorageView['status'] }) {
  switch (status) {
    case 'ready':
      return (
        <span className="n-pill n-pill-ok">
          <span className="n-pill-dot" />
          ready
        </span>
      )
    case 'deploying':
      return (
        <span className="n-pill n-pill-warn">
          <span className="n-pill-dot" />
          deploying
        </span>
      )
    case 'deleting':
      return (
        <span className="n-pill n-pill-warn">
          <span className="n-pill-dot" />
          deleting
        </span>
      )
    case 'error':
      return (
        <span className="n-pill n-pill-err">
          <span className="n-pill-dot" />
          error
        </span>
      )
  }
}

function DeployPanel({ onDeployed }: { onDeployed: (s: S3StorageView) => void }) {
  const [diskGB, setDiskGB] = useState(50)
  const [deploying, setDeploying] = useState(false)
  const [progress, setProgress] = useState<S3DeployProgress[]>([])
  const [error, setError] = useState<string | null>(null)

  const handleDeploy = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setProgress([])
    setDeploying(true)
    try {
      const result = await deployS3Storage(diskGB, (evt) => {
        setProgress((prev) => [...prev, evt])
      })
      onDeployed(result)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Deploy failed')
    } finally {
      setDeploying(false)
    }
  }

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Deploy S3 storage
        </span>
        <span
          className="n-pill"
          style={{
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
        >
          not deployed
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Deploys a dedicated VM running MinIO (an S3-compatible object store)
        on the cluster. The VM is provisioned through Nimbus's normal flow,
        then bootstrapped over SSH. Buckets and root credentials become
        available after MinIO comes up — usually 3-5 minutes.
      </p>

      <form onSubmit={handleDeploy} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="n-field">
          <label className="n-label" htmlFor="s3-disk">
            Disk size: <strong>{diskGB} GB</strong>
            <span style={{ marginLeft: 8, fontSize: 11, color: 'var(--ink-mute)', fontWeight: 400 }}>
              (10-120 GB; online grow is a future feature)
            </span>
          </label>
          <input
            id="s3-disk"
            type="range"
            min={10}
            max={120}
            step={10}
            value={diskGB}
            onChange={(e) => setDiskGB(Number(e.target.value))}
            disabled={deploying}
            style={{ width: '100%' }}
          />
        </div>

        {progress.length > 0 && (
          <div
            style={{
              padding: 12,
              background: 'rgba(20,18,28,0.03)',
              border: '1px solid var(--line)',
              borderRadius: 8,
              fontSize: 13,
            }}
          >
            <div style={{ fontWeight: 600, marginBottom: 6 }}>Progress</div>
            <ul style={{ listStyle: 'none', padding: 0, margin: 0 }}>
              {progress.map((evt, i) => (
                <li key={i} style={{ color: 'var(--ink-body)', padding: '2px 0' }}>
                  ✓ {PROGRESS_LABELS[evt.step] ?? evt.label}
                </li>
              ))}
              {deploying && (
                <li style={{ color: 'var(--ink-mute)', padding: '2px 0' }}>
                  … installing Docker + MinIO (this step takes 1-3 min)
                </li>
              )}
            </ul>
          </div>
        )}

        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}

        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button
            type="submit"
            className="n-btn n-btn-primary"
            disabled={deploying}
            style={{ minWidth: 140 }}
          >
            {deploying ? 'Deploying…' : 'Deploy storage'}
          </button>
        </div>
      </form>
    </div>
  )
}

function StatusPanel({ storage, onDelete }: { storage: S3StorageView; onDelete: () => void }) {
  const [showCreds, setShowCreds] = useState(false)
  const [confirming, setConfirming] = useState(false)

  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          S3 storage
        </span>
        <StatusPill status={storage.status} />
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 10, fontSize: 13 }}>
        <Row label="Endpoint" value={storage.endpoint ?? '—'} mono />
        <Row label="Console" value={storage.endpoint ? storage.endpoint.replace(':9000', ':9001') : '—'} mono />
        <Row label="Node" value={storage.node} />
        <Row label="VMID" value={String(storage.vmid)} />
        <Row label="Disk" value={`${storage.disk_gb} GB`} />
        {storage.error_msg && (
          <Row label="Error" value={storage.error_msg} valueStyle={{ color: 'var(--err)' }} />
        )}
      </div>

      <div className="n-divider" />

      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <span style={{ fontSize: 13, fontWeight: 600 }}>Root credentials</span>
          <button type="button" className="n-btn n-btn-ghost" onClick={() => setShowCreds((s) => !s)}>
            {showCreds ? 'Hide' : 'Reveal'}
          </button>
        </div>
        {showCreds ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            <Row label="Access key" value={storage.root_user ?? '—'} mono />
            <Row label="Secret key" value={storage.root_password ?? '—'} mono />
            <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--ink-mute)' }}>
              These are MinIO's root credentials — full administrative access.
              Keep them out of source control. Per-app keys (with bucket scoping)
              land in a follow-up release.
            </p>
          </div>
        ) : (
          <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)' }}>
            Hidden by default. Click Reveal to show.
          </p>
        )}
      </div>

      <div className="n-divider" />

      <div style={{ display: 'flex', gap: 12 }}>
        <button
          type="button"
          className="n-btn"
          onClick={() => setConfirming(true)}
          disabled={storage.status === 'deleting'}
          style={{ color: 'var(--err)', borderColor: 'var(--err)' }}
        >
          Delete storage
        </button>
      </div>

      {confirming && (
        <DeleteS3Confirm
          storage={storage}
          onConfirm={() => deleteS3Storage()}
          onCancel={() => setConfirming(false)}
          onDeleted={() => {
            setConfirming(false)
            onDelete()
          }}
        />
      )}
    </div>
  )
}

function Row({
  label,
  value,
  mono,
  valueStyle,
}: {
  label: string
  value: string
  mono?: boolean
  valueStyle?: React.CSSProperties
}) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12 }}>
      <span style={{ color: 'var(--ink-mute)' }}>{label}</span>
      <span
        style={{
          fontFamily: mono ? 'var(--font-mono, ui-monospace, SFMono-Regular, monospace)' : undefined,
          color: 'var(--ink)',
          textAlign: 'right',
          wordBreak: 'break-all',
          ...valueStyle,
        }}
      >
        {value}
      </span>
    </div>
  )
}

// BucketsPointer replaces the old in-place admin bucket CRUD. Per-user
// prefixed buckets now live at /buckets so every bucket has an owner; the
// admin manages buckets through their own user account too.
function BucketsPointer({ disabled }: { disabled: boolean }) {
  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}
    >
      <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>Buckets</span>
      {disabled ? (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          MinIO is not ready yet. Buckets become manageable once status flips to “ready”.
        </p>
      ) : (
        <>
          <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
            Bucket management moved to a per-user surface — every bucket is
            owned by a Nimbus user (admins included) and gets a stable name
            prefix to keep namespaces from colliding.
          </p>
          <Link
            to="/buckets"
            className="n-btn n-btn-secondary"
            style={{ alignSelf: 'flex-start' }}
          >
            Open buckets →
          </Link>
        </>
      )}
    </div>
  )
}

export default function S3() {
  const [storage, setStorage] = useState<S3StorageView | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)

  const refresh = async () => {
    try {
      setStorage(await getS3Storage())
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : 'Failed to load storage')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    refresh()
  }, [])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px', display: 'inline-flex', alignItems: 'center', gap: 10 }}>
          S3 storage
          <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-0.5 rounded">
            Alpha
          </span>
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          A self-hosted MinIO server, S3-API compatible. Deploy once; create
          buckets and use them from any S3 client (aws-cli, mc, boto3,
          rclone, etc.).
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          {loading ? (
            <div className="glass" style={{ padding: '24px 28px' }}>
              <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
            </div>
          ) : loadError ? (
            <div className="glass" style={{ padding: '24px 28px' }}>
              <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{loadError}</p>
            </div>
          ) : storage ? (
            <>
              <StatusPanel storage={storage} onDelete={() => setStorage(null)} />
              <BucketsPointer disabled={storage.status !== 'ready'} />
            </>
          ) : (
            <DeployPanel onDeployed={(s) => setStorage(s)} />
          )}
        </div>
      </div>
    </div>
  )
}
