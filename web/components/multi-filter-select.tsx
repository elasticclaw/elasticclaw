"use client"

import { ChevronDown } from "lucide-react"
import { Checkbox } from "@/components/ui/checkbox"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

export function selectedFilterValues(value?: string) {
  return value?.split(",").map((item) => {
    try {
      return decodeURIComponent(item)
    } catch {
      return item.trim()
    }
  }).filter(Boolean) ?? []
}

export function serializeFilterValues(values: string[]) {
  return values.map((value) => encodeURIComponent(value)).join(",")
}

export function MultiFilterSelect({
  label,
  value,
  values = [],
  onChange,
}: {
  label: string
  value?: string
  values?: string[]
  onChange: (value?: string) => void
}) {
  const selected = selectedFilterValues(value)
  const selectedSet = new Set(selected)
  const summary = selected.length === 0 ? "All" : selected.length === 1 ? selected[0] : `${selected.length} selected`

  const toggle = (item: string) => {
    const next = selectedSet.has(item)
      ? selected.filter((selectedItem) => selectedItem !== item)
      : [...selected, item]
    onChange(next.length > 0 ? serializeFilterValues(next) : undefined)
  }

  return (
    <Popover>
      <PopoverTrigger asChild>
        <button type="button" className="flex h-8 w-full items-center justify-between gap-1 rounded-md border border-input bg-input/30 px-2.5 text-xs shadow-xs outline-none transition-[color,box-shadow] hover:bg-input/50 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50">
          <span className="truncate"><span className="text-muted-foreground">{label}:</span> <span className={selected.length === 0 ? "text-muted-foreground" : "text-foreground"}>{summary}</span></span>
          <ChevronDown className="size-3 shrink-0 text-muted-foreground" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--radix-popover-trigger-width)] min-w-[158px] p-1">
        <div className="max-h-56 overflow-y-auto">
          {values.length === 0 ? (
            <p className="px-2 py-1.5 text-sm text-muted-foreground">No options</p>
          ) : (
            values.map((item) => (
              <label key={item} className="flex cursor-pointer items-center gap-2 rounded-sm px-2 py-1.5 text-sm hover:bg-accent">
                <Checkbox checked={selectedSet.has(item)} onCheckedChange={() => toggle(item)} />
                <span className="min-w-0 truncate">{item}</span>
              </label>
            ))
          )}
        </div>
      </PopoverContent>
    </Popover>
  )
}
