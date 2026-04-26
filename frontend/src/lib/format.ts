export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = bytes
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}

// buildSSHCommand renders a copy-pasteable ssh invocation. keyName omits the
// `-i` flag when blank; port omits `-p` when blank or 22 (default).
export function buildSSHCommand(
  user: string,
  host: string,
  keyName?: string,
  port?: number,
): string {
  const parts = ['ssh']
  if (keyName) parts.push(`-i ~/.ssh/${keyName}`)
  if (port && port !== 22) parts.push(`-p ${port}`)
  parts.push(`${user}@${host}`)
  return parts.join(' ')
}

// parseTunnelURL splits a "host:port" string emitted by the Gopher tunnel
// flow. Returns undefined for empty or malformed input — callers should
// fall back to showing the raw value.
export function parseTunnelURL(url: string): { host: string; port: number } | undefined {
  if (!url) return undefined
  const idx = url.lastIndexOf(':')
  if (idx <= 0) return undefined
  const host = url.slice(0, idx)
  const port = parseInt(url.slice(idx + 1), 10)
  if (!Number.isFinite(port) || port <= 0 || port > 65535) return undefined
  return { host, port }
}
