"use client"

import { Activity, BarChart3, BookOpen, ChevronDown, Database, GitBranch, Plus, RefreshCw, Ticket, TriangleAlert } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import type { AIChatSource, AIChatSources } from "@/lib/api"
import { chatModes, type ChatMode } from "./mode-cards"
import { groupSources, sourceStatus, sourceSummary } from "./sources"

export function ChatHeader({ mode, title, data, loading = !data, onNewChat, onRefreshSources }: {
  mode: ChatMode | null
  title?: string
  data?: AIChatSources
  loading?: boolean
  onNewChat: () => void
  onRefreshSources: () => void
}) {
  const sources = data?.sources ?? []
  const summary = sourceSummary(sources, loading)
  const modeName = chatModes.find((item) => item.id === mode)?.name
  // py-2.5 around h-8 controls matches the sidebar and board headers so the hairlines line up.
  return (
    <header className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
      <h1 className="shrink-0 whitespace-nowrap text-sm font-semibold">AI Chat</h1>
      {modeName && <span className="shrink-0 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs text-muted-foreground">{modeName}</span>}
      {title && <p className="min-w-0 truncate text-sm text-muted-foreground">{title}</p>}
      <div className="ml-auto flex items-center gap-2">
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="outline" size="sm" className="gap-2" disabled={!data} aria-label={`${summary.label} · read-only`} aria-busy={loading}>
              <span className={`size-1.5 rounded-full ${summary.dotClass}`} aria-hidden="true" />
              {summary.label} <span className="text-muted-foreground">· read-only</span><ChevronDown className="size-3" />
            </Button>
          </PopoverTrigger>
          <PopoverContent align="end" className="max-h-[var(--radix-popover-content-available-height)] w-[460px] max-w-[calc(100vw-2rem)] overflow-y-auto rounded-xl p-5">
            <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
              <h2 className="text-sm font-semibold">What this chat can read</h2>
              <span className="text-xs text-muted-foreground">Workspace {data?.workspace}</span>
            </div>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">The assistant reads these and never changes them.</p>
            {sources.length === 0 && <p className="mt-5 text-xs text-muted-foreground">No sources configured.</p>}
            {groupSources(sources).map((group) => (
              <section key={group.name} className="mt-5">
                <h3 className="mb-2.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{group.name}</h3>
                <div className="space-y-5">{group.sources.map((source, index) => renderSource(source, index))}</div>
              </section>
            ))}
            <div className="mt-5 flex items-start gap-3 border-t pt-3">
              <p className="flex-1 text-[11px] leading-relaxed text-muted-foreground">Set in the workspace configuration, the same for everyone in this workspace.</p>
              <Button variant="ghost" size="sm" className="h-6 shrink-0 gap-1.5 px-1 text-xs text-muted-foreground" disabled={loading} onClick={onRefreshSources}>
                <RefreshCw className={`size-3 ${loading ? "animate-spin" : ""}`} />Refresh
              </Button>
            </div>
          </PopoverContent>
        </Popover>
        <Button variant="outline" size="sm" onClick={onNewChat}><Plus className="size-3.5" />New chat</Button>
      </div>
    </header>
  )
}

function renderSource(source: AIChatSource, index: number) {
  const icons: Record<string, typeof Database> = { knowledge_base: BookOpen, repositories: GitBranch, posthog: BarChart3, datadog: Activity, issue_tracker: Ticket }
  const Icon = Object.hasOwn(icons, source.kind) ? icons[source.kind] : Database
  const status = sourceStatus(source.status)
  const repository = source.kind === "knowledge_base" || source.kind === "repositories"
  const [names, ...detail] = (source.detail ?? "").split(" · ")
  return (
    <div key={`${source.kind}-${index}`} className="flex items-start gap-2.5">
      <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-muted"><Icon className="size-3.5" aria-hidden="true" /></span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="text-xs font-semibold">{source.name || source.kind || "Source"}</span>
          <span className="rounded border px-1.5 py-0.5 text-[10px] leading-none text-muted-foreground">Read</span>
          <span className={`ml-auto flex items-center gap-1.5 whitespace-nowrap text-[11px] ${status.textClass}`}>
            {status.warning ? <TriangleAlert className="size-3" aria-hidden="true" /> : <span className={`size-1.5 rounded-full ${status.dotClass}`} aria-hidden="true" />}
            {status.label}
          </span>
        </div>
        {source.detail && <p className="mt-1 break-words text-xs leading-relaxed text-muted-foreground">
          {repository ? <><span className="font-mono text-[11px]">{names}</span>{detail.length > 0 && ` · ${detail.join(" · ")}`}</> : source.detail}
        </p>}
        {source.error && <p className={`mt-1 break-words text-xs leading-relaxed ${status.textClass}`}>{source.error}</p>}
      </div>
    </div>
  )
}
