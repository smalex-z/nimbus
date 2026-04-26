import Button from './Button'

// PAGE_SIZE_OPTIONS is the ladder offered by the page-size dropdown.
// Callers default to 10 — keeps the table compact at first load; users can
// step up the dropdown when they need more rows on screen.
export const PAGE_SIZE_OPTIONS = [10, 25, 50, 100] as const

interface PaginationProps {
  total: number
  pageSize: number
  page: number // zero-indexed
  onPageChange: (p: number) => void
  onPageSizeChange?: (n: number) => void
  pageSizeOptions?: readonly number[]
}

// Pagination renders a compact "1-25 of 142" summary plus prev/next controls
// and an optional page-size dropdown. Designed to sit directly under a table
// inside the same Card so the chrome reads as one component.
//
// Returns null when total fits in a single page AND the size selector is
// disabled — no point taking vertical space when there's nothing to page.
export default function Pagination({
  total,
  pageSize,
  page,
  onPageChange,
  onPageSizeChange,
  pageSizeOptions = PAGE_SIZE_OPTIONS,
}: PaginationProps) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const start = total === 0 ? 0 : page * pageSize + 1
  const end = Math.min(total, (page + 1) * pageSize)

  if (total <= pageSize && !onPageSizeChange) return null

  return (
    <div className="flex items-center justify-between gap-3 px-4 py-3 border-t border-line font-mono text-[11px] text-ink-3 flex-wrap">
      <span className="uppercase tracking-wider">
        {total === 0 ? 'no entries' : `${start}–${end} of ${total}`}
      </span>
      <div className="flex items-center gap-2">
        {onPageSizeChange && (
          <select
            className="rounded-[8px] bg-white/85 font-mono text-[11px] text-ink border border-line-2 px-2 py-1 focus:outline-none uppercase tracking-wider"
            value={pageSize}
            onChange={(e) => onPageSizeChange(Number(e.target.value))}
            aria-label="rows per page"
          >
            {pageSizeOptions.map((n) => (
              <option key={n} value={n}>
                {n} / page
              </option>
            ))}
          </select>
        )}
        <Button
          variant="ghost"
          size="small"
          disabled={page === 0}
          onClick={() => onPageChange(page - 1)}
        >
          ‹ prev
        </Button>
        <span className="uppercase tracking-wider whitespace-nowrap">
          page {page + 1} / {totalPages}
        </span>
        <Button
          variant="ghost"
          size="small"
          disabled={page >= totalPages - 1}
          onClick={() => onPageChange(page + 1)}
        >
          next ›
        </Button>
      </div>
    </div>
  )
}
