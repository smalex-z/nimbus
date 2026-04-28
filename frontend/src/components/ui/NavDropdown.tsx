import { KeyboardEvent, ReactNode, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'

type Placement = 'bottom-end' | 'right-start' | 'left-start'

// triggerOn controls how the menu opens. 'hover' (default) matches the
// existing nav menus where a mouseEnter pops the panel; 'click' suppresses
// the hover handlers entirely so the menu only opens on the trigger button's
// click. Per-row dropdowns inside dense tables use 'click' to avoid
// accidentally opening every row the cursor passes over.
type TriggerOn = 'hover' | 'click'

interface NavDropdownProps {
  trigger: ReactNode
  triggerClassName?: string
  panelClassName?: string
  wrapperClassName?: string
  placement?: Placement
  triggerOn?: TriggerOn
  children: ReactNode
}

const HOVER_CLOSE_DELAY_MS = 150

export default function NavDropdown({
  trigger,
  triggerClassName,
  panelClassName,
  wrapperClassName = 'inline-block',
  placement = 'bottom-end',
  triggerOn = 'hover',
  children,
}: NavDropdownProps) {
  const [open, setOpen] = useState(false)
  const wrapperRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const panelRef = useRef<HTMLDivElement | null>(null)
  const closeTimer = useRef<number | null>(null)

  // Trigger position, recomputed every time we render the click-mode panel
  // so it tracks scroll/resize. Used only when triggerOn==='click', which
  // portals the panel to <body> with position:fixed — necessary because
  // ancestor `.glass` cards establish a stacking context (backdrop-filter)
  // that traps an absolute-positioned panel inside the row.
  const [triggerRect, setTriggerRect] = useState<DOMRect | null>(null)
  const portalMode = triggerOn === 'click'

  useEffect(() => {
    if (!open) return
    const handleDocClick = (e: MouseEvent) => {
      const target = e.target as Node
      if (wrapperRef.current?.contains(target)) return
      // In portal mode the panel is outside the wrapper — also exempt clicks
      // inside the panel itself so a menu-item button can fire before close.
      if (panelRef.current?.contains(target)) return
      setOpen(false)
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

  // Recompute the portal panel's position whenever it opens, and keep it in
  // sync with viewport scroll/resize so the menu doesn't drift away from
  // its trigger when the user scrolls the table behind it.
  useLayoutEffect(() => {
    if (!open || !portalMode) return
    const update = () => {
      if (triggerRef.current) {
        setTriggerRect(triggerRef.current.getBoundingClientRect())
      }
    }
    update()
    window.addEventListener('scroll', update, true)
    window.addEventListener('resize', update)
    return () => {
      window.removeEventListener('scroll', update, true)
      window.removeEventListener('resize', update)
    }
  }, [open, portalMode])

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

  const hoverProps = portalMode
    ? {}
    : {
        onMouseEnter: () => {
          cancelClose()
          setOpen(true)
        },
        onMouseLeave: scheduleClose,
      }

  const panelClasses = `min-w-[180px] rounded-[10px] bg-white shadow-lg border border-line py-1 ${panelClassName ?? ''}`

  // Portal mode: render the panel at fixed coordinates derived from the
  // trigger's bounding rect. right-anchored under the trigger so a row's
  // menu lines up with its … button regardless of where in the viewport
  // the row currently sits.
  const portalPanel =
    portalMode && open && triggerRect
      ? createPortal(
          <div
            ref={panelRef}
            role="menu"
            style={{
              position: 'fixed',
              top: triggerRect.bottom + 4,
              right: window.innerWidth - triggerRect.right,
              zIndex: 1000,
            }}
            className={panelClasses}
          >
            {children}
          </div>,
          document.body,
        )
      : null

  return (
    <div
      ref={wrapperRef}
      className={`relative ${wrapperClassName}`}
      {...hoverProps}
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

      {!portalMode && open && (
        <div role="menu" className={`absolute z-30 ${panelPositionClass} ${panelClasses}`}>
          {children}
        </div>
      )}
      {portalPanel}
    </div>
  )
}
