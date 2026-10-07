"use client"

import { ArrowUp } from "lucide-react"
import { Button } from "@/components/ui/button"

// The first slice renders the composer without accepting or sending messages.
export function ChatComposer() {
  return (
    <div className="rounded-xl border bg-card p-3">
      <textarea aria-label="Message" placeholder="Ask anything..." disabled className="min-h-24 w-full resize-none bg-transparent text-sm outline-none disabled:cursor-not-allowed" />
      <div className="flex items-center justify-end gap-3">
        <span className="text-xs text-muted-foreground">Messaging is coming soon</span>
        <Button disabled size="icon" className="size-8 rounded-full" aria-label="Send message"><ArrowUp className="size-4" /></Button>
      </div>
    </div>
  )
}
