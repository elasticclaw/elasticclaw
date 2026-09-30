"use client"

import { ArrowUp, Square } from "lucide-react"
import { Button } from "@/components/ui/button"

export function ChatComposer({ value, onChange, onSend, onCancel, streaming, disabled }: {
  value: string
  onChange: (value: string) => void
  onSend: () => void
  onCancel: () => void
  streaming: boolean
  disabled?: boolean
}) {
  return (
    <div className="rounded-xl border bg-card p-3">
      <textarea aria-label="Message" placeholder="Ask anything..." value={value} onChange={(event) => onChange(event.target.value)} disabled={disabled || streaming}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
            event.preventDefault()
            if (value.trim() && !disabled && !streaming) onSend()
          }
        }} className="min-h-24 w-full resize-none bg-transparent text-sm outline-none disabled:opacity-60" />
      <div className="flex items-center justify-end gap-3">
        <span className="text-xs text-muted-foreground">Enter to send · Shift+Enter for a new line</span>
        {streaming ? <Button size="icon" variant="outline" onClick={onCancel} className="size-8 rounded-full" aria-label="Cancel response"><Square className="size-3" /></Button> :
          <Button disabled={disabled || !value.trim()} onClick={onSend} size="icon" className="size-8 rounded-full" aria-label="Send message"><ArrowUp className="size-4" /></Button>}
      </div>
    </div>
  )
}
