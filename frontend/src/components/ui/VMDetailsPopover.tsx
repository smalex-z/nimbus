import { useState } from 'react'
import { humanizeOSTemplate } from '@/lib/os'
import type { ClusterVM } from '@/types'

// VMDetailsPopover wraps a VM-table cell with a hover-triggered popover that
// surfaces fields the row itself doesn't have room for: full pretty OS name,
// kernel, machine arch, raw ostype hint, IP source, and the underlying source
// (local / foreign / external).
//
// Pure CSS hover would be simpler but breaks the moment the popover overflows
// a `overflow-x-auto` ancestor (which the VM table has). State + portal-less
// fixed positioning would be more robust; this component leans on `relative`
// + `absolute` and trusts the consumer not to clip it. Good enough for the
// admin table where the popover is small.
interface Props {
  vm: ClusterVM
  children: React.ReactNode
}

export default function VMDetailsPopover({ vm, children }: Props) {
  const [open, setOpen] = useState(false)

  const rows: Array<{ label: string; value: string }> = []

  if (vm.os_pretty) {
    rows.push({ label: 'OS', value: vm.os_pretty })
  } else if (vm.os_template) {
    rows.push({ label: 'OS', value: humanizeOSTemplate(vm.os_template) })
  }
  if (vm.os_kernel) rows.push({ label: 'Kernel', value: vm.os_kernel })
  if (vm.os_machine) rows.push({ label: 'Arch', value: vm.os_machine })
  if (vm.ip) {
    rows.push({
      label: 'IP',
      value: vm.ip_source ? `${vm.ip} (${vm.ip_source})` : vm.ip,
    })
  }
  rows.push({ label: 'Source', value: humanizeSource(vm.source) })
  if (vm.tier) rows.push({ label: 'Tier', value: vm.tier })
  if (vm.hostname && vm.hostname !== vm.name) rows.push({ label: 'Hostname', value: vm.hostname })
  if (vm.username) rows.push({ label: 'User', value: vm.username })
  rows.push({ label: 'VMID', value: String(vm.vmid) })
  rows.push({ label: 'Node', value: vm.node })

  return (
    <span
      className="relative inline-block"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
    >
      {children}
      {open && (
        <div className="absolute z-20 left-0 top-full mt-1 w-72 rounded-[10px] bg-white shadow-lg border border-line p-3 text-left pointer-events-none">
          <div className="font-display text-sm font-medium mb-2">{vm.name}</div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px]">
            {rows.map((row) => (
              <div key={row.label} className="contents">
                <dt className="font-mono uppercase tracking-wider text-ink-3">{row.label}</dt>
                <dd className="font-mono text-ink break-words">{row.value}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </span>
  )
}

function humanizeSource(source: ClusterVM['source']): string {
  switch (source) {
    case 'local': return 'Nimbus (this instance)'
    case 'foreign': return 'Nimbus (other instance)'
    case 'external': return 'External (manual)'
  }
}
