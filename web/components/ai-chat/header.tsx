"use client"

import { ChevronDown, Plus } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import type { AIChatSources } from "@/lib/api"
import { chatModes, type ChatMode } from "./mode-cards"

export function ChatHeader({ mode, title, data, onNewChat, onRefreshSources }: {
  mode: ChatMode | null
  title?: string
  data?: AIChatSources
  onNewChat: () => void
  onRefreshSources: () => void
}) {
  const sources = data?.sources ?? []
  return (
    <header className="flex min-h-14 shrink-0 items-center gap-3 border-b px-5 py-2.5">
      <h1 className="text-sm font-semibold">AI Chat</h1>
      <span className="rounded-full border px-2 py-0.5 text-xs text-muted-foreground">{chatModes.find((item) => item.id === mode)?.name ?? "Choose a mode"}</span>
      {title && <p className="min-w-0 truncate text-sm text-muted-foreground">{title}</p>}
      <div className="ml-auto flex items-center gap-2">
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="outline" size="sm" className="gap-2" disabled={!data}>
              <span className="size-1.5 rounded-full bg-muted-foreground" />
              {sources.length} sources <span className="text-muted-foreground">· read-only</span><ChevronDown className="size-3" />
            </Button>
          </PopoverTrigger>
          <PopoverContent align="end" className="w-80 space-y-3">
            <p className="text-sm font-medium">Workspace sources</p>
            {sources.length === 0 && <p className="text-xs text-muted-foreground">No sources configured.</p>}
            {sources.map((source, index) => (
              <div key={`${source.kind}-${index}`} className="text-xs">
                <div className="flex justify-between gap-3"><span className="break-all">{source.name}</span><span className={source.status === "invalid" ? "text-destructive" : "text-muted-foreground"}>{source.status === "invalid" ? "Invalid" : "Unchecked"}</span></div>
                {source.error && <p className="mt-1 text-destructive">{source.error}</p>}
              </div>
            ))}
            <p className="text-xs text-muted-foreground">Connections have not been checked yet.</p>
            <Button variant="outline" size="sm" onClick={onRefreshSources}>Refresh sources</Button>
          </PopoverContent>
        </Popover>
        <Button variant="outline" size="sm" onClick={onNewChat}><Plus className="size-3.5" />New chat</Button>
      </div>
    </header>
  )
}
