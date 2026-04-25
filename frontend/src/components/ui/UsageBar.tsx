export default function UsageBar({ label, pct, hint }: { label: string; pct: number; hint: string }) {
  return (
    <div>
      <div className="flex justify-between text-[11px] font-mono text-ink-3 mb-1.5 uppercase tracking-wider">
        <span>{label}</span>
        <span className="normal-case tracking-normal">{hint}</span>
      </div>
      <div className="h-2.5 rounded-md bg-[rgba(27,23,38,0.06)] overflow-hidden">
        <div
          className="h-full rounded-md"
          style={{
            width: `${Math.min(100, pct).toFixed(0)}%`,
            background: 'linear-gradient(90deg, var(--c1, #F8AF82), var(--c2, #F496B4))',
          }}
        />
      </div>
    </div>
  )
}
