import { cn } from '@/lib/utils'

export type Severity = 'TRACE' | 'DEBUG' | 'INFO' | 'WARN' | 'ERROR' | 'FATAL'

export const SEVERITY: Record<Severity, { rank: number; color: string; bg: string }> = {
  TRACE: { rank: 1, color: 'var(--muted-foreground)', bg: 'var(--muted)' },
  DEBUG: { rank: 5, color: 'var(--muted-foreground)', bg: 'var(--muted)' },
  INFO: { rank: 9, color: 'var(--chart-1)', bg: 'color-mix(in srgb, var(--chart-1) 15%, transparent)' },
  WARN: { rank: 13, color: 'var(--chart-3)', bg: 'color-mix(in srgb, var(--chart-3) 15%, transparent)' },
  ERROR: { rank: 17, color: 'var(--chart-4)', bg: 'color-mix(in srgb, var(--chart-4) 15%, transparent)' },
  FATAL: { rank: 21, color: '#fff', bg: 'var(--destructive)' },
}

export function SeverityChip({ severity, sev, className }: { severity?: Severity; sev?: Severity; className?: string }) {
  const value = severity ?? sev ?? 'INFO'
  const state = SEVERITY[value] ?? SEVERITY.INFO
  return <span title={`severity_text=${value} · severity_number=${state.rank}`} className={cn('inline-flex w-[46px] shrink-0 items-center justify-center rounded-sm py-px font-mono text-[10px] font-medium tracking-[0.03em]', className)} style={{ color: state.color, backgroundColor: state.bg }}>{value}</span>
}
