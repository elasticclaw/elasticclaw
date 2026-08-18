'use client'

import * as React from 'react'
import { format, isSameDay, startOfMonth, subDays } from 'date-fns'
import { CalendarDays, ChevronDown } from 'lucide-react'
import type { DateRange } from 'react-day-picker'

import { Button } from '@/components/ui/button'
import { Calendar } from '@/components/ui/calendar'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { cn } from '@/lib/utils'

export interface DatePickerRangeProps {
  value?: DateRange
  onChange: (range: DateRange | undefined) => void
  className?: string
}

const PRESETS: { id: string; name: string; days?: number }[] = [
  { id: '7d', name: 'Last 7 days', days: 7 },
  { id: '30d', name: 'Last 30 days', days: 30 },
  { id: '90d', name: 'Last 90 days', days: 90 },
  { id: 'mtd', name: 'Month to date' },
]

function presetRange(preset: { days?: number }): DateRange {
  const to = new Date()
  return { from: preset.days ? subDays(to, preset.days) : startOfMonth(to), to }
}

/* The analytics default period when the URL carries no from/to — kept here so
   the trigger can resolve it to the "Last 30 days" preset label. */
export function defaultPeriod(): DateRange {
  return presetRange(PRESETS[1])
}

function activePreset(range?: DateRange) {
  if (!range?.from || !range.to) return undefined
  return PRESETS.find((preset) => {
    const candidate = presetRange(preset)
    return isSameDay(candidate.from!, range.from!) && isSameDay(candidate.to!, range.to!)
  })
}

/* The current period is always readable as text — the active preset's name, or
   the resolved dates — instead of inferred from which button looks active. */
function rangeLabel(range?: DateRange) {
  const preset = activePreset(range)
  if (preset) return preset.name
  if (!range?.from) return 'Select date range'
  if (!range.to) return format(range.from, 'MMM d, yyyy')
  return `${format(range.from, 'MMM d')} – ${format(range.to, 'MMM d, yyyy')}`
}

export function DatePickerRange({ value, onChange, className }: DatePickerRangeProps) {
  const [open, setOpen] = React.useState(false)
  const active = activePreset(value)

  // Picking a start day keeps the popover open until an end day is chosen; a
  // click over an already-complete range starts a fresh one instead of
  // extending `to` (react-day-picker's default).
  function selectRange(range: DateRange | undefined, day: Date) {
    if (value?.from && value.to) {
      onChange({ from: day, to: undefined })
      return
    }
    onChange(range)
    if (range?.from && range.to) setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className={cn('justify-start font-normal', className)}>
          <CalendarDays className="size-3.5 text-muted-foreground" />
          <span className="text-muted-foreground">Period:</span>
          <span className="font-medium">{rangeLabel(value)}</span>
          <ChevronDown className="size-3.5 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="flex w-auto p-0" align="start">
        <div className="flex min-w-37 flex-col gap-0.5 border-r p-2">
          {PRESETS.map((preset) => (
            <button
              key={preset.id}
              type="button"
              onClick={() => { onChange(presetRange(preset)); setOpen(false) }}
              className={cn(
                'rounded-md px-2 py-1.5 text-left text-xs',
                active?.id === preset.id ? 'bg-muted font-medium text-foreground' : 'text-muted-foreground hover:bg-muted/50',
              )}
            >
              {preset.name}
            </button>
          ))}
          <div className="my-1 border-t" />
          <span className="px-2 font-mono text-[11px] text-muted-foreground">{value?.from ? format(value.from, 'yyyy-MM-dd') : '—'}</span>
          <span className="px-2 font-mono text-[11px] text-muted-foreground">{value?.to ? format(value.to, 'yyyy-MM-dd') : '…'}</span>
        </div>
        <Calendar
          mode="range"
          selected={value}
          onSelect={selectRange}
          numberOfMonths={1}
          defaultMonth={value?.to ?? value?.from}
          className="[&_[data-range-middle=true]]:!bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] [&_[data-range-middle=true]]:!text-foreground"
        />
      </PopoverContent>
    </Popover>
  )
}

export { DatePickerRange as DatePicker }
