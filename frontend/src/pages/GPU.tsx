import { useEffect, useMemo, useState } from 'react'
import {
  cancelGPUJob,
  getGPUInference,
  getGPUJob,
  listGPUJobs,
} from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import { formatRelativeTime } from '@/lib/format'
import type { GPUInferenceStatus, GPUJob, GPUJobStatus } from '@/types'

// Poll cadence for the jobs list. Two seconds is responsive enough that a
// queued job appearing after submit feels live, slow enough that a busy
// page doesn't pin the network. Detail polling (when an individual job is
// running) is faster — see DETAIL_POLL_MS below.
const LIST_POLL_MS = 3_000
const DETAIL_POLL_MS = 2_000

const STATUS_ORDER: GPUJobStatus[] = ['running', 'queued', 'failed', 'cancelled', 'succeeded']

export default function GPU() {
  const [jobs, setJobs] = useState<GPUJob[]>([])
  const [inference, setInference] = useState<GPUInferenceStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [openJob, setOpenJob] = useState<GPUJob | null>(null)

  // Top-level polling for jobs list + inference status.
  useEffect(() => {
    let cancelled = false
    const tick = () => {
      Promise.all([listGPUJobs(), getGPUInference()])
        .then(([js, inf]) => {
          if (!cancelled) {
            setJobs(js)
            setInference(inf)
            setError(null)
          }
        })
        .catch((e: unknown) => {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e))
        })
        .finally(() => {
          if (!cancelled) setLoading(false)
        })
    }
    tick()
    const id = setInterval(tick, LIST_POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [])

  // Detail polling for the open job. Only runs while the modal is open and
  // the job hasn't reached a terminal state.
  useEffect(() => {
    if (!openJob || isTerminal(openJob.status)) return
    let cancelled = false
    const tick = () => {
      getGPUJob(openJob.id)
        .then((j) => { if (!cancelled) setOpenJob(j) })
        .catch(() => {/* swallow — modal stays on last known state */})
    }
    const id = setInterval(tick, DETAIL_POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [openJob])

  const grouped = useMemo(() => {
    const m = new Map<GPUJobStatus, GPUJob[]>()
    for (const s of STATUS_ORDER) m.set(s, [])
    for (const j of jobs) {
      const arr = m.get(j.status)
      if (arr) arr.push(j)
    }
    return m
  }, [jobs])

  return (
    <div>
      <div className="mb-8">
        <div className="eyebrow">GPU plane</div>
        <h2 className="text-3xl inline-flex items-center gap-2.5">
          Jobs
          <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-0.5 rounded">
            Alpha
          </span>
        </h2>
        <p className="text-base text-ink-2 mt-2">
          Monitor training jobs running on the GX10. Submit jobs from your VM
          with{' '}
          <code className="font-mono text-sm bg-[rgba(27,23,38,0.05)] px-1.5 py-0.5 rounded">
            gx10 submit &lt;image&gt; -- &lt;command&gt;
          </code>
          .
        </p>
      </div>

      <InferenceCard inference={inference} />

      {error && (
        <Card className="mt-6 p-4 text-bad text-sm">Failed to load jobs: {error}</Card>
      )}

      {loading ? (
        <Card className="mt-6 p-6 text-ink-3 font-mono text-sm">Loading…</Card>
      ) : (
        <div className="mt-6 space-y-6">
          {STATUS_ORDER.map((s) => {
            const rows = grouped.get(s) ?? []
            if (rows.length === 0) return null
            return (
              <JobsSection
                key={s}
                status={s}
                jobs={rows}
                onOpen={(j) => setOpenJob(j)}
              />
            )
          })}
          {jobs.length === 0 && (
            <Card className="p-12 text-center">
              <div className="eyebrow">No jobs yet</div>
              <p className="text-sm text-ink-2 mt-2">
                SSH into a VM and run{' '}
                <code className="font-mono text-xs bg-[rgba(27,23,38,0.05)] px-1.5 py-0.5 rounded">
                  gx10 submit
                </code>{' '}
                to queue your first job.
              </p>
            </Card>
          )}
        </div>
      )}

      {openJob && <JobDetailModal job={openJob} onClose={() => setOpenJob(null)} onCancel={async () => {
        try {
          const updated = await cancelGPUJob(openJob.id)
          setOpenJob(updated)
          setJobs((prev) => prev.map((j) => (j.id === updated.id ? updated : j)))
        } catch (e) {
          // The modal will keep the prior state on failure — the next poll
          // tick will refresh.
          console.error('cancel failed', e)
        }
      }} />}
    </div>
  )
}

function isTerminal(s: GPUJobStatus): boolean {
  return s === 'succeeded' || s === 'failed' || s === 'cancelled'
}

function InferenceCard({ inference }: { inference: GPUInferenceStatus | null }) {
  if (!inference) return null
  const status = inference.status
  const statusTone =
    status === 'up' ? 'text-good' : status === 'down' ? 'text-warn' : 'text-ink-3'
  return (
    <Card className="p-5 flex items-center gap-4">
      <div className="brand-mark" aria-hidden />
      <div className="flex-1 min-w-0">
        <div className="eyebrow">Inference server</div>
        <div className="font-display text-lg truncate">{inference.model || 'no model configured'}</div>
        <div className="font-mono text-xs text-ink-3 truncate">
          {inference.base_url || 'unconfigured — visit Settings → GPU'}
        </div>
      </div>
      <div className={`font-mono text-[11px] uppercase tracking-wider ${statusTone}`}>
        {status}
      </div>
    </Card>
  )
}

function JobsSection({
  status,
  jobs,
  onOpen,
}: {
  status: GPUJobStatus
  jobs: GPUJob[]
  onOpen: (j: GPUJob) => void
}) {
  return (
    <div>
      <div className="eyebrow mb-2">
        {status} ({jobs.length})
      </div>
      <Card className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-line">
              {['ID', 'Image', 'Command', 'Worker', 'When'].map((c) => (
                <th
                  key={c}
                  className="text-left text-[11px] font-mono uppercase tracking-wider text-ink-3 px-4 py-3 whitespace-nowrap"
                >
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr
                key={j.id}
                className="border-t border-line hover:bg-[rgba(27,23,38,0.02)] cursor-pointer"
                onClick={() => onOpen(j)}
              >
                <td className="px-4 py-3 font-mono text-xs text-ink-2">#{j.id}</td>
                <td className="px-4 py-3 font-mono text-xs text-ink truncate max-w-xs">{j.image}</td>
                <td className="px-4 py-3 font-mono text-xs text-ink-2 truncate max-w-md">
                  {j.command || <span className="text-ink-3">— image default —</span>}
                </td>
                <td className="px-4 py-3 font-mono text-xs text-ink-3">{j.worker_id || '—'}</td>
                <td className="px-4 py-3 font-mono text-xs text-ink-3 whitespace-nowrap">
                  {formatRelativeTime(j.finished_at || j.started_at || j.queued_at)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
    </div>
  )
}

function JobDetailModal({
  job,
  onClose,
  onCancel,
}: {
  job: GPUJob
  onClose: () => void
  onCancel: () => void
}) {
  const canCancel = !isTerminal(job.status)
  return (
    <div
      className="fixed inset-0 z-50 bg-black/40 flex items-center justify-center p-6"
      onClick={onClose}
    >
      <div
        className="bg-white rounded-[12px] shadow-2xl w-full max-w-3xl max-h-[80vh] flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="p-5 border-b border-line flex items-center justify-between">
          <div>
            <div className="eyebrow">Job #{job.id}</div>
            <div className="font-display text-lg">{job.image}</div>
            <div className="font-mono text-xs text-ink-3 mt-1">
              status: <span className="text-ink-2">{job.status}</span>
              {job.exit_code != null && <span> · exit: {job.exit_code}</span>}
              {job.worker_id && <span> · worker: {job.worker_id}</span>}
            </div>
          </div>
          <div className="flex gap-2">
            {canCancel && (
              <Button variant="danger" size="small" onClick={onCancel}>
                Cancel
              </Button>
            )}
            <Button variant="ghost" size="small" onClick={onClose}>
              Close
            </Button>
          </div>
        </div>

        {job.command && (
          <div className="px-5 pt-3">
            <div className="eyebrow mb-1">Command</div>
            <pre className="font-mono text-xs bg-[rgba(27,23,38,0.04)] rounded-[6px] p-3 whitespace-pre-wrap break-all">{job.command}</pre>
          </div>
        )}

        {job.error_msg && (
          <div className="px-5 pt-3">
            <div className="eyebrow mb-1 text-bad">Error</div>
            <pre className="font-mono text-xs text-bad whitespace-pre-wrap">{job.error_msg}</pre>
          </div>
        )}

        <div className="flex-1 min-h-0 px-5 py-3 overflow-hidden flex flex-col">
          <div className="eyebrow mb-1">Log tail</div>
          <pre className="font-mono text-[11px] bg-[rgba(20,18,28,0.85)] text-white rounded-[6px] p-3 overflow-auto flex-1 whitespace-pre-wrap break-all">
            {job.log_tail || <span className="text-ink-3">— no output yet —</span>}
          </pre>
        </div>
      </div>
    </div>
  )
}
