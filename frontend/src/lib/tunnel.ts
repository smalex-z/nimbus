// Shared formatting helpers for Gopher tunnel rows. Used by both TunnelsModal
// (per-port tunnels surface on a VM) and MyVMs (cards listing every public
// exposure on each machine), so the two views agree on what a tunnel's
// public-facing string looks like.

import type { VMTunnel } from '@/api/client'

// formatTunnelPublic returns the user-facing host string for a Gopher tunnel.
// Priority:
//   1. tunnel.tunnel_url       — set by Gopher for HTTP tunnels with a
//                                subdomain (e.g. "https://foo.altsuite.co").
//   2. "<host>:<server_port>"  — port-only TCP, UDP, or no-subdomain HTTP.
//                                Gopher returns server_port for these but
//                                no tunnel_url.
//   3. tunnel.subdomain        — degraded fallback, sometimes the only
//                                non-id field present on edge-case responses.
//   4. tunnel.id               — last-resort hex id; almost always means we
//                                haven't fetched /api/tunnels/info yet.
export function formatTunnelPublic(t: VMTunnel, host: string | undefined): string {
  if (t.tunnel_url) return t.tunnel_url
  if (host && t.server_port) return `${host}:${t.server_port}`
  if (t.subdomain) return t.subdomain
  return t.id
}

// formatTunnelMapping is the "<public> → localhost:<target>" string used in
// the My machines card and anywhere else we want to show *both* sides of a
// Gopher exposure at a glance.
export function formatTunnelMapping(t: VMTunnel, host: string | undefined): string {
  return `${formatTunnelPublic(t, host)} → localhost:${t.target_port}`
}
