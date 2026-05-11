import { useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

// Console renders a full-screen xterm.js terminal connected to the VM's
// serial console via the /api/vms/{id}/console/ws relay. Opens in a new
// tab from the SSH details modal. Login uses the one-time noVNC console
// password Nimbus generated at provision time.
export default function Console() {
  const { id } = useParams<{ id: string }>()
  const containerRef = useRef<HTMLDivElement | null>(null)
  const [status, setStatus] = useState<'connecting' | 'open' | 'closed' | 'error'>('connecting')
  const [errorMsg, setErrorMsg] = useState<string | null>(null)

  useEffect(() => {
    if (!id || !containerRef.current) return
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      theme: { background: '#0f0e15', foreground: '#e8e6f0' },
      convertEol: true,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()

    // Same-origin WS — Nimbus serves the SPA + the API on one host, so a
    // relative URL with the right scheme is enough. wss when the page is
    // https; ws otherwise.
    const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${scheme}//${window.location.host}/api/vms/${id}/console/ws`)
    ws.binaryType = 'arraybuffer'

    const decoder = new TextDecoder()
    ws.onopen = () => {
      setStatus('open')
      term.focus()
    }
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        term.write(ev.data)
      } else {
        term.write(decoder.decode(ev.data as ArrayBuffer))
      }
    }
    ws.onclose = (ev) => {
      setStatus('closed')
      // The relay sends a close frame with a useful reason on upstream
      // failure; surface it inline so the user knows why the session
      // ended (VM not running, agent disabled, etc.) without digging in
      // the journal.
      if (ev.reason) setErrorMsg(ev.reason)
      term.writeln('')
      term.writeln(`\x1b[31m[connection closed${ev.reason ? `: ${ev.reason}` : ''}]\x1b[0m`)
    }
    ws.onerror = () => {
      setStatus('error')
    }

    // Browser keystrokes → upstream. xterm hands us the raw escape
    // sequence; the Proxmox serial channel takes the same bytes.
    const dataDisposable = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(data)
      }
    })

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    return () => {
      window.removeEventListener('resize', onResize)
      dataDisposable.dispose()
      ws.close()
      term.dispose()
    }
  }, [id])

  return (
    <div className="h-screen w-screen flex flex-col bg-[#0f0e15] text-white">
      <header className="flex items-center justify-between px-4 py-2 border-b border-white/10 text-xs font-mono">
        <div>
          <span className="text-white/60">VM #{id} serial console</span>
          {errorMsg && <span className="ml-3 text-red-400">{errorMsg}</span>}
        </div>
        <div className="flex items-center gap-2">
          <span
            className={
              status === 'open'
                ? 'text-emerald-400'
                : status === 'connecting'
                  ? 'text-amber-400'
                  : 'text-red-400'
            }
          >
            ● {status}
          </span>
        </div>
      </header>
      <div ref={containerRef} className="flex-1 overflow-hidden p-2" />
    </div>
  )
}
