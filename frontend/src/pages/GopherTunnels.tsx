import GopherPanel from '@/components/ui/GopherPanel'

// GopherTunnels — Infrastructure → Gopher tunnels page. The actual
// UI + state lives in GopherPanel (shared with the install wizard so
// the two surfaces stay in lockstep). This page is just the
// page-level header + layout shell.
export default function GopherTunnels() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Gopher tunnels
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Configure the reverse-tunnel gateway Nimbus uses to expose VMs at
          public hostnames.
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2 flex flex-col gap-6">
          <GopherPanel />
        </div>
      </div>
    </div>
  )
}
