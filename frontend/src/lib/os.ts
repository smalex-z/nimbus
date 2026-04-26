// OS family / label helpers used by the admin VM table. Lives here (not in
// the component files) so the components can be edited under React Fast
// Refresh without burning the page state.

export type IconFamily =
  | 'ubuntu'
  | 'debian'
  | 'fedora'
  | 'centos'
  | 'arch'
  | 'alpine'
  | 'windows'
  | 'linux'
  | 'unknown'

// resolveOSId picks an icon family from the best signal we have. Inputs in
// priority order:
//   1. agentId — qemu-guest-agent osinfo `id` ("ubuntu", "debian", "mswindows", …)
//   2. template — Nimbus os_template ("ubuntu-22.04")
//   3. ostype — Proxmox raw ostype hint ("l26", "win10")
export function resolveOSId(args: {
  agentId?: string
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

  const ostype = (args.ostype || '').toLowerCase()
  if (ostype.startsWith('win')) return 'windows'
  if (ostype === 'l24' || ostype === 'l26') return 'linux'
  return 'unknown'
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
