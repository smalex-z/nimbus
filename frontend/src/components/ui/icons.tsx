// Shared inline SVG icons for VM-row affordances. Kept here (rather than
// pulling in a real icon library) so the SPA stays small and tree-shakable;
// each icon defaults to currentColor so callers control color via Tailwind
// text-* utilities. Default size is 14×14 to match the 7×7-rounded-md button
// affordances; pass size to override for the smaller menu glyphs.

interface IconProps {
  size?: number
  className?: string
}

const baseSvgProps = {
  fill: 'none',
  stroke: 'currentColor',
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
}

// NetworkIcon — globe-with-meridians. Used for the per-VM Gopher tunnels
// affordance on both the Admin cluster table and My Machines.
export function NetworkIcon({ size = 14, className }: IconProps = {}) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" strokeWidth={1.5} className={className} {...baseSvgProps}>
      <circle cx="8" cy="8" r="6" />
      <ellipse cx="8" cy="8" rx="3" ry="6" />
      <path d="M2 8h12" />
    </svg>
  )
}

// RestartIcon — circular arrow. Reads as "send me back through boot".
export function RestartIcon({ size = 13, className }: IconProps = {}) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" strokeWidth={1.6} className={className} {...baseSvgProps}>
      <path d="M14 8a6 6 0 1 1-1.76-4.24" />
      <path d="M14 2.5V5.5H11" />
    </svg>
  )
}

// ForceStopIcon — filled square. Distinct from the round ShutdownIcon to
// signal "pull the plug, no graceful pass first".
export function ForceStopIcon({ size = 13, className }: IconProps = {}) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" className={className} aria-hidden>
      <rect x="3.5" y="3.5" width="9" height="9" rx="1.5" fill="currentColor" />
    </svg>
  )
}

// TrashIcon — destructive Remove glyph. Same path as the legacy admin
// trash icon for visual continuity with anyone who used the old delete
// button.
export function TrashIcon({ size = 13, className }: IconProps = {}) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" strokeWidth={1.6} className={className} {...baseSvgProps}>
      <path d="M3 4h10" />
      <path d="M6 4V2.75A.75.75 0 0 1 6.75 2h2.5a.75.75 0 0 1 .75.75V4" />
      <path d="M4.5 4l.75 9a1 1 0 0 0 1 .9h3.5a1 1 0 0 0 1-.9L11.5 4" />
      <path d="M6.5 7v4M9.5 7v4" />
    </svg>
  )
}
