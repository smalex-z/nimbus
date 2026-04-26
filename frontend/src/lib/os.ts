// OS family / label helpers used by the admin VM table. Lives here (not in
// the component files) so the components can be edited under React Fast
// Refresh without burning the page state.

// IconFamily covers the distros we draw distinct icons for, plus two generic
// fallbacks:
//   - 'linux'  — we know it's Linux but not which distro (e.g. external VM
//                with `ostype: l26` and no agent). Renders as Tux.
//   - 'other'  — we don't know what's inside at all. Renders as a generic
//                server-stack to avoid implying Linux.
export type IconFamily =
  | 'ubuntu'
  | 'debian'
  | 'fedora'
  | 'centos'
  | 'arch'
  | 'alpine'
  | 'windows'
  | 'linux'
  | 'other'

// resolveOSId picks an icon family from the best signal we have. Inputs in
// priority order:
//   1. agentId — qemu-guest-agent osinfo `id` ("ubuntu", "debian", "mswindows", …)
//   2. name — VM name; lets us pattern-match agentless external VMs that the
//      operator helpfully named "kevin-debian" or similar. Last-resort.
//   3. template — Nimbus os_template ("ubuntu-22.04")
//   4. ostype — Proxmox raw ostype hint ("l26", "win10")
export function resolveOSId(args: {
  agentId?: string
  name?: string
  template?: string
  ostype?: string
}): IconFamily {
  const id = (args.agentId || '').toLowerCase()
  if (id) {
    if (id.includes('ubuntu')) return 'ubuntu'
    if (id.includes('debian')) return 'debian'
    if (id.includes('fedora')) return 'fedora'
    if (id.includes('centos') || id.includes('rhel') || id.includes('rocky') || id.includes('alma')) return 'centos'
    if (id.includes('arch')) return 'arch'
    if (id.includes('alpine')) return 'alpine'
    if (id.includes('mswindows') || id.includes('windows')) return 'windows'
  }
  const tmpl = (args.template || '').toLowerCase()
  if (tmpl.startsWith('ubuntu')) return 'ubuntu'
  if (tmpl.startsWith('debian')) return 'debian'
  if (tmpl.startsWith('fedora')) return 'fedora'
  if (tmpl.startsWith('alpine')) return 'alpine'
  if (tmpl.startsWith('arch')) return 'arch'

  // Heuristic on the VM name as a last resort. A user who names a VM
  // "kevin-debian" is telling us what's in it. We only match whole-word
  // tokens (separated by `-`, `_`, `.`, or whitespace) to avoid false hits
  // on names like "ubuntukubernetes-test".
  const name = (args.name || '').toLowerCase()
  if (/(^|[-_. ])(ubuntu|ubu)([-_. ]|$)/.test(name)) return 'ubuntu'
  if (/(^|[-_. ])(debian|deb)([-_. ]|$)/.test(name)) return 'debian'
  if (/(^|[-_. ])fedora([-_. ]|$)/.test(name)) return 'fedora'
  if (/(^|[-_. ])(centos|rocky|alma|rhel)([-_. ]|$)/.test(name)) return 'centos'
  if (/(^|[-_. ])arch([-_. ]|$)/.test(name)) return 'arch'
  if (/(^|[-_. ])alpine([-_. ]|$)/.test(name)) return 'alpine'
  if (/(^|[-_. ])(windows|win)([-_. ]|$)/.test(name)) return 'windows'

  const ostype = (args.ostype || '').toLowerCase()
  if (ostype.startsWith('win')) return 'windows'
  if (ostype === 'l24' || ostype === 'l26') return 'linux'
  return 'other'
}

// humanizeOSTemplate renders a Nimbus os_template like "ubuntu-22.04" as
// "Ubuntu 22.04". Falls back to the raw value for anything unrecognized
// (e.g. a Proxmox ostype hint like "l26" or "win10").
export function humanizeOSTemplate(value: string): string {
  const m = /^(ubuntu|debian|fedora|centos|alma|rocky|arch|alpine)[-_](.+)$/i.exec(value)
  if (m) {
    const distro = m[1][0].toUpperCase() + m[1].slice(1).toLowerCase()
    return `${distro} ${m[2]}`
  }
  if (/^win/i.test(value)) return value.toUpperCase().replace('WIN', 'Windows ')
  if (value === 'l24' || value === 'l26') return 'Linux'
  return value
}
