"use client"

import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select"

const anyValue = "__any__"

function formatLabel(value: string) {
  return value.replace(/_/g, " ").replace(/\b\w/g, (match) => match.toUpperCase())
}

export function FilterSelect({ label, value, values, onChange }: { label: string; value?: string; values?: string[]; onChange: (value?: string) => void }) {
  const selectValues = value && !(values ?? []).includes(value)
    ? [value, ...(values ?? [])]
    : (values ?? [])
  const displayValue = value ? formatLabel(value) : "any"

  return (
    <Select value={value ?? anyValue} onValueChange={(next) => onChange(next === anyValue ? undefined : next)}>
      <SelectTrigger size="sm" className="w-full bg-background">
        <span className="truncate">
          <span className="text-muted-foreground">{label}:</span> {displayValue}
        </span>
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={anyValue}>{label}: any</SelectItem>
        {selectValues.map((item) => (
          <SelectItem key={item} value={item}>{formatLabel(item)}</SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}
