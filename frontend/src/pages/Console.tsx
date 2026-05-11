import { useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import RFB from '@novnc/novnc'
import { openConsoleSession } from '@/api/client'

// Console renders a full-screen graphical console (noVNC) connected to
// the VM via Proxmox's vncwebsocket. PVE rejects API token auth at
// termproxy's in-band ticket check (PVE 9.x bug); vncproxy does not,
// so we use the graphical path. Login is at the VM's regular getty —
// same one-time noVNC password Nimbus generated at provision time.
//
// Two-step bootstrap: POST /console/session opens a vncproxy session
// on the PVE node and returns ticket+port; the browser then opens the
// WS with those as query params, and noVNC uses the ticket as the VNC
// password during the RFB auth phase.
export default function Console() {
  const { id } = useParams<{ id: string }>()
  const containerRef = useRef<HTMLDivElement | null>(null)
  const rfbRef = useRef<RFB | null>(null)
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected' | 'error'>(
    'connecting',
  )
  const [errorMsg, setErrorMsg] = useState<string | null>(null)

  useEffect(() => {
    if (!id || !containerRef.current) return
    const vmId = id
    let cancelled = false

    ;(async () => {
      try {
        const session = await openConsoleSession(Number(vmId))
        if (cancelled || !containerRef.current) return
        const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
        const wsUrl =
          `${scheme}//${window.location.host}/api/vms/${vmId}/console/ws` +
          `?port=${session.port}&vncticket=${encodeURIComponent(session.ticket)}`
        const rfb = new RFB(containerRef.current, wsUrl, {
          credentials: { password: session.ticket },
          wsProtocols: ['binary'],
        })
        rfb.viewOnly = false
        rfb.scaleViewport = true
        rfb.resizeSession = false
        rfb.background = '#0f0e15'
        rfb.addEventListener('connect', () => setStatus('connected'))
        rfb.addEventListener('disconnect', (e: Event) => {
          setStatus('disconnected')
          const detail = (e as CustomEvent<{ clean: boolean }>).detail
          if (detail && !detail.clean) setErrorMsg('connection closed unexpectedly')
        })
        rfb.addEventListener('securityfailure', (e: Event) => {
          setStatus('error')
          const detail = (e as CustomEvent<{ status: number; reason: string }>).detail
          setErrorMsg(`VNC auth rejected: ${detail?.reason || `status ${detail?.status}`}`)
        })
        rfb.addEventListener('credentialsrequired', () => {
          setErrorMsg('VNC server asked for additional credentials beyond the ticket')
        })
        rfbRef.current = rfb
      } catch (err: unknown) {
        if (cancelled) return
        setStatus('error')
        setErrorMsg(err instanceof Error ? err.message : String(err))
      }
    })()

    return () => {
      cancelled = true
      if (rfbRef.current) {
        try {
          rfbRef.current.disconnect()
        } catch {
          // ignore — disconnect on a half-built RFB throws
        }
        rfbRef.current = null
      }
    }
  }, [id])

  return (
    <div className="h-screen w-screen flex flex-col bg-[#0f0e15] text-white">
      <header className="flex items-center justify-between px-4 py-2 border-b border-white/10 text-xs font-mono">
        <div>
          <span className="text-white/60">VM #{id} console</span>
          {errorMsg && <span className="ml-3 text-red-400">{errorMsg}</span>}
        </div>
        <span
          className={
            status === 'connected'
              ? 'text-emerald-400'
              : status === 'connecting'
                ? 'text-amber-400'
                : 'text-red-400'
          }
        >
          ● {status}
        </span>
      </header>
      <div ref={containerRef} className="flex-1 overflow-hidden" />
    </div>
  )
}
