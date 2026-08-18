'use client'

import * as React from 'react'
import { format, isSameDay, startOfMonth, subDays } from 'date-fns'
import { CalendarIcon } from 'lucide-react'
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

const PRESETS = [
  { label: '7d', name: 'Last 7 days', days: 7 },
  { label: '30d', name: 'Last 30 days', days: 30 },
  { label: '90d', name: 'Last 90 days', days: 90 },
] as const

function presetRange(days: number): DateRange {
  const to = new Date()
  return { from: subDays(to, days - 1), to }
}

function rangeLabel(range?: DateRange) {
  if (!range?.from) return 'Select date range'
  if (!range.to) return format(range.from, 'MMM d, yyyy')
  return `${format(range.from, 'MMM d')} – ${format(range.to, 'MMM d, yyyy')}`
}

export function DatePickerRange({ value, onChange, className }: DatePickerRangeProps) {
  const [open, setOpen] = React.useState(false)

  function selectRange(range: DateRange | undefined) {
    onChange(range)
    if (range?.from && range.to && value?.from && !isSameDay(range.from, range.to)) setOpen(false)
  }

  function selectPreset(days: number) {
    onChange(presetRange(days))
    setOpen(false)
  }

  function selectMonthToDate() {
    onChange({ from: startOfMonth(new Date()), to: new Date() })
    setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className={cn('justify-start font-normal', className)}>
          <CalendarIcon className="size-4" />
          {rangeLabel(value)}
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-auto p-0" align="start">
        <div className="flex gap-1 border-b p-2">
          {PRESETS.map((preset) => (
            <Button key={preset.label} variant="ghost" size="sm" title={preset.name} onClick={() => selectPreset(preset.days)}>
              {preset.label}
            </Button>
          ))}
          <Button variant="ghost" size="sm" title="Month to date" onClick={selectMonthToDate}>
            MTD
          </Button>
        </div>
        <Calendar
          mode="range"
          selected={value}
          onSelect={selectRange}
          numberOfMonths={1}
          defaultMonth={value?.from}
          className="[&_[data-range-middle=true]]:!bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] [&_[data-range-middle=true]]:!text-foreground"
        />
      </PopoverContent>
    </Popover>
  )
}

export { DatePickerRange as DatePicker }
