// Inline SVG icons for the most common guest OSes the cluster sees.
// Brand-recognizable shapes (Ubuntu's circle-of-friends, Debian's spiral,
// Windows' four squares, Tux for generic Linux, server-stack for "other")
// rendered monochrome so they inherit the surrounding text color.
import type { IconFamily } from '@/lib/os'

interface OSIconProps {
  family: IconFamily
  size?: number
  className?: string
}

export default function OSIcon({ family, size = 14, className = '' }: OSIconProps) {
  const common = {
    width: size,
    height: size,
    viewBox: '0 0 24 24',
    className: `inline-block flex-shrink-0 ${className}`,
    'aria-hidden': true,
  }

  switch (family) {
    case 'ubuntu':
      // Circle of friends: open ring + three "heads" at 12/4/8 o'clock.
      return (
        <svg {...common} fill="currentColor">
          <circle cx="12" cy="12" r="9.25" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <circle cx="12" cy="3" r="2.1" />
          <circle cx="20.4" cy="16.5" r="2.1" />
          <circle cx="3.6" cy="16.5" r="2.1" />
        </svg>
      )
    case 'debian':
      // Stylized spiral inside a near-closed arc — Debian's signature swirl.
      return (
        <svg {...common} fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
          <path d="M19 12a7 7 0 1 1-7-7" />
          <path d="M9 12a3.5 3.5 0 1 0 6 2.5" />
          <path d="M14 9.5a2.5 2.5 0 1 0-2 4" />
        </svg>
      )
    case 'fedora':
      // Filled disc with an inset "f" cutout (the Fedora wordmark).
      return (
        <svg {...common} fill="currentColor">
          <path d="M12 2a10 10 0 1 0 10 10A10 10 0 0 0 12 2zm3 4.5h-3a3 3 0 0 0-3 3v2H7.5v3H9v5h3v-5h2.5v-3H12v-1.5a.5.5 0 0 1 .5-.5H15z" />
        </svg>
      )
    case 'centos':
      // Four-leaf rose: the CentOS "vault" pattern, simplified to four wedges.
      return (
        <svg {...common} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round">
          <path d="M12 3v6m0 6v6M3 12h6m6 0h6" />
          <rect x="9" y="9" width="6" height="6" transform="rotate(45 12 12)" />
        </svg>
      )
    case 'arch':
      // Triangle outline — Arch's hollow chevron.
      return (
        <svg {...common} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round">
          <path d="M12 3 3 21l4.5-2c2.5-1 6.5-1 9 0L21 21z" />
        </svg>
      )
    case 'alpine':
      // Two mountain peaks.
      return (
        <svg {...common} fill="currentColor">
          <path d="M2 20l5.5-9 3.5 5 3-3 8 7z" />
        </svg>
      )
    case 'windows':
      // Four-square Windows logo (Windows 8+ era — the most recognizable).
      return (
        <svg {...common} fill="currentColor">
          <rect x="3" y="3" width="8.5" height="8.5" />
          <rect x="12.5" y="3" width="8.5" height="8.5" />
          <rect x="3" y="12.5" width="8.5" height="8.5" />
          <rect x="12.5" y="12.5" width="8.5" height="8.5" />
        </svg>
      )
    case 'linux':
      // Tux silhouette — used when we know it's Linux but not which distro
      // (typical for external VMs without qemu-guest-agent). Distinct from
      // 'other', which signals "we genuinely don't know what this is".
      return (
        <svg {...common} fill="currentColor">
          <ellipse cx="12" cy="15" rx="5.5" ry="6.5" />
          <circle cx="12" cy="6.5" r="3.7" />
          <circle cx="10.6" cy="6.2" r="0.55" fill="#fff" />
          <circle cx="13.4" cy="6.2" r="0.55" fill="#fff" />
          <path d="M10.8 8.1 12 9.3l1.2-1.2-.6-.4h-1.2z" fill="#f5a623" />
          <path d="M7 19l-1 3h2l1-2zM17 19l1 3h-2l-1-2z" />
        </svg>
      )
    default:
      // 'other' / 'unknown' — a generic compute/server stack so it reads as
      // "some kind of machine, but we can't tell what's inside" rather than
      // implying Linux.
      return (
        <svg {...common} fill="none" stroke="currentColor" strokeWidth="1.5">
          <rect x="3" y="4" width="18" height="5" rx="1" />
          <rect x="3" y="11" width="18" height="5" rx="1" />
          <rect x="3" y="18" width="18" height="3" rx="1" />
          <circle cx="6" cy="6.5" r="0.7" fill="currentColor" stroke="none" />
          <circle cx="6" cy="13.5" r="0.7" fill="currentColor" stroke="none" />
        </svg>
      )
  }
}
