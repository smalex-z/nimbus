// Inline SVG icons for the most common guest OSes the cluster sees.
// We deliberately keep them simple monochrome marks rather than brand-accurate
// logos — the goal is "at-a-glance distribution recognition", not marketing.
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
    fill: 'currentColor',
    'aria-hidden': true,
  }

  switch (family) {
    case 'ubuntu':
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="10" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <circle cx="20" cy="12" r="2" />
          <circle cx="8" cy="6.5" r="2" />
          <circle cx="8" cy="17.5" r="2" />
        </svg>
      )
    case 'debian':
      return (
        <svg {...common}>
          <path d="M12 2a10 10 0 1 0 10 10A10 10 0 0 0 12 2zm0 2a8 8 0 0 1 7.42 5h-2.13A6 6 0 1 0 12 18a6 6 0 0 0 5.94-5h2.06A8 8 0 1 1 12 4z" />
          <path d="M14 9.5a3.5 3.5 0 1 1-3.5-3.5 2.6 2.6 0 0 1 3.5 3.5z" fill="none" stroke="currentColor" strokeWidth="1.2" />
        </svg>
      )
    case 'fedora':
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="10" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M12 7h3a3 3 0 0 1 0 6h-2v4h-2v-6h4a1 1 0 0 0 0-2h-3a3 3 0 0 0-3 3v5H7V12a5 5 0 0 1 5-5z" />
        </svg>
      )
    case 'centos':
      return (
        <svg {...common}>
          <path d="M12 2l2 5h5l-4 3 1.5 6L12 13l-4.5 3L9 10 5 7h5z" fill="none" stroke="currentColor" strokeWidth="1.4" />
        </svg>
      )
    case 'arch':
      return (
        <svg {...common}>
          <path d="M12 2L4 21l8-4 8 4z" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
        </svg>
      )
    case 'alpine':
      return (
        <svg {...common}>
          <path d="M3 19l5-8 4 5 3-3 6 6z" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
        </svg>
      )
    case 'windows':
      return (
        <svg {...common}>
          <path d="M3 5l8-1v8H3zM12 4l9-1v10h-9zM3 13h8v8l-8-1zM12 13h9v8l-9-1z" />
        </svg>
      )
    case 'linux':
      return (
        <svg {...common}>
          <path d="M12 3a4 4 0 0 0-4 4v3.5c-1.5 1-3 3-3 6 0 .8.4 1.4 1 1.7L8 20h8l2-1.8c.6-.3 1-.9 1-1.7 0-3-1.5-5-3-6V7a4 4 0 0 0-4-4zm-1.5 4a.8.8 0 1 1 0 1.6.8.8 0 0 1 0-1.6zm3 0a.8.8 0 1 1 0 1.6.8.8 0 0 1 0-1.6zM12 11c1.5 0 2.5 1 2.5 2H9.5c0-1 1-2 2.5-2z" />
        </svg>
      )
    default:
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M9 9a3 3 0 1 1 4.5 2.6c-.8.5-1.5 1-1.5 2v.4M12 17v.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      )
  }
}
