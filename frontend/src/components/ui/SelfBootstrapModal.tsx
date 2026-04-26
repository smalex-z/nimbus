import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  getSelfBootstrapStatus,
  startSelfBootstrap,
  type SelfBootstrapState,
} from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'

interface SelfBootstrapModalProps {
  onClose: () => void
}

// Phases the orchestrator walks through. Order matches the state machine
// in internal/selftunnel/service.go; the modal renders all five with the
// current phase highlighted.
const PHASES: { state: SelfBootstrapState; label: string }[] = [
  { state: 'registering', label: 'Registering Nimbus host with Gopher' },
  { state: 'installing', label: 'Installing rathole client locally' },
  { state: 'waiting_connect', label: 'Waiting for tunnel to connect' },
  { state: 'creating_tunnel', label: 'Creating cloud subdomain tunnel' },
  { state: 'active', label: 'Cloud tunnel active' },
]

const POLL_INTERVAL_MS = 2_000
const REDIRECT_AFTER_MS = 5_000

// SelfBootstrapModal polls /settings/gopher/self-bootstrap and renders the
// state machine as a phase checklist. On `active`, shows the new public
// URL with a "Open" button + a 5s auto-redirect. On `failed`, shows the
// error and a "Try again" button.
export default function SelfBootstrapModal({ onClose }: SelfBootstrapModalProps) {
  const [state, setState] = useState<SelfBootstrapState>('')
  const [error, setError] = useState<string | undefined>()
  const [tunnelURL, setTunnelURL] = useState<string | undefined>()
  const [retrying, setRetrying] = useState(false)
  const [redirectIn, setRedirectIn] = useState<number | null>(null)
  const redirectedRef = useRef(false)

  // Poll until terminal state (active or failed). Polling stops on close.
  useEffect(() => {
    let cancelled = false
    const tick = async () => {
      try {
        const s = await getSelfBootstrapStatus()
        if (cancelled) return
        setState(s.state)
        setError(s.error)
        setTunnelURL(s.tunnel_url)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : 'failed to load status')
      }
    }
    void tick()
    const id = setInterval(tick, POLL_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [retrying])

  // Auto-redirect once tunnel is active. Only fires once even if the modal
  // re-renders during the countdown.
  useEffect(() => {
    if (state !== 'active' || !tunnelURL || redirectedRef.current) return
    setRedirectIn(Math.ceil(REDIRECT_AFTER_MS / 1000))
    const id = setInterval(() => {
      setRedirectIn((n) => {
        if (n === null) return null
        if (n <= 1) {
          clearInterval(id)
          if (!redirectedRef.current) {
            redirectedRef.current = true
            window.location.href = tunnelURL
          }
          return 0
        }
        return n - 1
      })
    }, 1_000)
    return () => clearInterval(id)
  }, [state, tunnelURL])

  // Close on Escape; lock body scroll while open.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])

  const onRetry = async () => {
    setError(undefined)
    setState('')
    setRetrying((n) => !n) // re-runs the polling effect
    try {
      await startSelfBootstrap()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to start bootstrap')
    }
  }

  const activeIdx = activePhaseIndex(state)
  const isFailed = state === 'failed'
  const isActive = state === 'active'

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label="Setting up Nimbus cloud tunnel"
    >
      <Card
        strong
        className="w-full max-w-[640px] max-h-[calc(100vh-2rem)] overflow-y-auto p-10"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-6">
          <div className="min-w-0">
            <div className="eyebrow">Cloud tunnel</div>
            <h3 className="text-3xl mt-1">
              {isActive ? 'Nimbus is online.' : 'Setting up your cloud URL…'}
            </h3>
            <p className="text-sm text-ink-2 mt-2 leading-relaxed">
              {isActive
                ? 'Your dashboard is now reachable from anywhere.'
                : 'Bootstrapping the host through Gopher and exposing the dashboard at a public hostname. This usually takes 60–90 seconds.'}
            </p>
          </div>
          {!isActive && (
            <button
              type="button"
              onClick={onClose}
              aria-label="Close"
              className="text-ink-3 hover:text-ink text-2xl leading-none p-1 -m-1 flex-shrink-0"
            >
              ×
            </button>
          )}
        </div>

        {!isActive && (
          <div className="mt-7">
            {PHASES.map((phase, i) => {
              const status: 'done' | 'active' | 'pending' =
                isFailed && i === activeIdx
                  ? 'active'
                  : i < activeIdx
                    ? 'done'
                    : i === activeIdx
                      ? 'active'
                      : 'pending'
              return (
                <PhaseRow
                  key={phase.state}
                  label={phase.label}
                  status={status}
                  failed={isFailed && i === activeIdx}
                />
              )
            })}
          </div>
        )}

        {isFailed && error && (
          <div className="mt-5 p-4 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.25)] text-bad text-[13px] leading-relaxed">
            <div className="font-medium mb-1">Bootstrap failed</div>
            <div className="font-mono whitespace-pre-line break-all">{error}</div>
          </div>
        )}

        {isActive && tunnelURL && (
          <div className="mt-7 p-5 rounded-[10px] bg-[rgba(45,125,90,0.06)] border border-[rgba(45,125,90,0.25)]">
            <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
              Public URL
            </div>
            <a
              href={tunnelURL}
              className="font-mono text-base text-ink underline break-all"
            >
              {tunnelURL}
            </a>
            {redirectIn !== null && redirectIn > 0 && (
              <p className="mt-3 text-[12px] text-ink-3">
                Redirecting in {redirectIn}s…
              </p>
            )}
          </div>
        )}

        <div className="flex justify-end gap-2 mt-9">
          {isFailed && (
            <Button variant="ghost" onClick={onRetry}>
              Try again
            </Button>
          )}
          {isActive && tunnelURL ? (
            <a href={tunnelURL}>
              <Button>Open Nimbus</Button>
            </a>
          ) : (
            <Button variant="ghost" onClick={onClose}>
              {isFailed ? 'Close' : 'Hide'}
            </Button>
          )}
        </div>
      </Card>
    </div>,
    document.body,
  )
}

function activePhaseIndex(state: SelfBootstrapState): number {
  switch (state) {
    case 'registering':
      return 0
    case 'installing':
      return 1
    case 'waiting_connect':
      return 2
    case 'creating_tunnel':
      return 3
    case 'active':
      return 4
    case 'failed':
      // Treat failed as "still on whatever step was active when it broke".
      // The orchestrator records state=failed without keeping the previous
      // phase, so we just point at step 0; the error message tells the
      // user what actually went wrong.
      return 0
    default:
      return -1
  }
}

interface PhaseRowProps {
  label: string
  status: 'done' | 'active' | 'pending'
  failed: boolean
}

function PhaseRow({ label, status, failed }: PhaseRowProps) {
  return (
    <div
      className={`flex items-center gap-3.5 py-3 border-b border-line text-sm last:border-b-0 ${
        status === 'pending' ? 'text-ink-3' : 'text-ink'
      } ${status === 'active' ? 'font-medium' : ''}`}
    >
      <span
        className={`w-[18px] h-[18px] rounded-full border-[1.5px] flex-shrink-0 grid place-items-center relative ${
          failed
            ? 'border-bad bg-[rgba(184,58,58,0.12)]'
            : status === 'pending'
              ? 'border-ink-3'
              : 'border-ink'
        } ${status === 'done' ? 'bg-ink' : ''}`}
      >
        {status === 'done' && <span className="text-white text-[10px] font-bold">✓</span>}
        {failed && <span className="text-bad text-[10px] font-bold">!</span>}
        {status === 'active' && !failed && (
          <span className="absolute inset-[3px] rounded-full bg-ink animate-blink" />
        )}
      </span>
      <span>{label}</span>
    </div>
  )
}
