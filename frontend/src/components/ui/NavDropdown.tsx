import { KeyboardEvent, ReactNode, useEffect, useRef, useState } from 'react'

type Placement = 'bottom-end' | 'right-start' | 'left-start'

interface NavDropdownProps {
  trigger: ReactNode
  triggerClassName?: string
  panelClassName?: string
  wrapperClassName?: string
  placement?: Placement
  children: ReactNode
}

const HOVER_CLOSE_DELAY_MS = 150

export default function NavDropdown({
  trigger,
  triggerClassName,
  panelClassName,
  wrapperClassName = 'inline-block',
  placement = 'bottom-end',
  children,
}: NavDropdownProps) {
  const [open, setOpen] = useState(false)
  const wrapperRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const closeTimer = useRef<number | null>(null)

  useEffect(() => {
    if (!open) return
    const handleDocClick = (e: MouseEvent) => {
      if (!wrapperRef.current?.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    document.addEventListener('mousedown', handleDocClick)
    return () => document.removeEventListener('mousedown', handleDocClick)
  }, [open])

  useEffect(() => {
    return () => {
      if (closeTimer.current !== null) {
        window.clearTimeout(closeTimer.current)
      }
    }
  }, [])

  const cancelClose = () => {
    if (closeTimer.current !== null) {
      window.clearTimeout(closeTimer.current)
      closeTimer.current = null
    }
  }

  const scheduleClose = () => {
    cancelClose()
    closeTimer.current = window.setTimeout(() => {
      setOpen(false)
      closeTimer.current = null
    }, HOVER_CLOSE_DELAY_MS)
  }

  const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape' && open) {
      e.stopPropagation()
      setOpen(false)
      triggerRef.current?.focus()
    }
  }

  // right-start: submenu opens flush to the right of its trigger.
  // left-start:  submenu opens flush to the left of its trigger.
  // bottom-end:  panel drops below the trigger, right-aligned.
  const panelPositionClass =
    placement === 'right-start'
      ? 'left-full top-0'
      : placement === 'left-start'
        ? 'right-full top-0'
        : 'right-0 top-full mt-1'

  return (
    <div
      ref={wrapperRef}
      className={`relative ${wrapperClassName}`}
      onMouseEnter={() => {
        cancelClose()
        setOpen(true)
      }}
      onMouseLeave={scheduleClose}
      onKeyDown={handleKeyDown}
    >
      <button
        ref={triggerRef}
        type="button"
        className={triggerClassName}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {trigger}
      </button>

      {open && (
        <div
          role="menu"
          className={`absolute z-30 ${panelPositionClass} min-w-[180px] rounded-[10px] bg-white shadow-lg border border-line py-1 ${panelClassName ?? ''}`}
        >
          {children}
        </div>
      )}
    </div>
  )
}
