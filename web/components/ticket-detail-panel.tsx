"use client"

import { CheckCircle2, ChevronRight, GitMerge, GitPullRequest, X, XCircle } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import type { AnalyticsTicket, AnalyticsTicketRunSummary, TaskRunAnalyticsFilters } from "@/lib/types"
import { fetchAnalyticsTicket } from "@/lib/api"
import { useEscapeToClose } from "@/hooks/use-escape-to-close"
import { useFocusTrap } from "@/hooks/use-focus-trap"
import { TicketStatusBadge } from "@/components/ds/ticket-status-badge"
import { RunStatusBadge } from "@/components/ds/run-status-badge"
import { Button } from "@/components/ui/button"

const usd = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 })
const caption = "text-[10px] font-medium uppercase tracking-[0.08em] text-muted-foreground"
const dateFormatter = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" })
const timeFormatter = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })
const duration = (ms?: number) => { if (!ms) return "—"; const s = Math.round(ms / 1000); if (s < 60) return `${s}s`; const minutes = Math.round(s / 60); if (minutes < 60) return `${minutes}m`; const hours = Math.floor(minutes / 60); return hours < 24 ? `${hours}h ${String(minutes % 60).padStart(2, "0")}m` : `${Math.floor(hours / 24)}d ${hours % 24}h` }
const date = (value?: number) => value ? dateFormatter.format(new Date(value)) : "—"
const time = (value?: number) => value ? timeFormatter.format(new Date(value)) : "—"

function Section({ title, stat, children }: { title: string; stat?: { left: string; right?: string; tone?: "error" }; children: React.ReactNode }) { return <section className="overflow-hidden rounded-lg border bg-card"><div className="flex items-center gap-3 border-b px-3 py-2"><h3 className="text-sm font-medium">{title}</h3>{stat && <div className="ml-auto flex items-center gap-3 font-mono text-[10px] text-muted-foreground"><span>{stat.left}</span>{stat.right && <span className={stat.tone === "error" ? "text-destructive" : ""}>{stat.right}</span>}</div>}</div><div className="space-y-2 p-3">{children}</div></section> }
function Row({ label, value, mono }: { label: string; value: React.ReactNode; mono?: boolean }) { return <div className="flex gap-3 py-0.5 text-sm"><span className={`${caption} w-27 shrink-0`}>{label}</span><span className={mono ? "min-w-0 break-all font-mono text-xs" : "min-w-0"}>{value}</span></div> }
const taskRunAnalyticsQuery = (filters?: TaskRunAnalyticsFilters) => {
  const params = new URLSearchParams()
  if (!filters) return ""
  const entries: Array<[string, string | number | boolean | undefined]> = [["status", filters.status], ["ownerType", filters.ownerType], ["workspace", filters.workspace], ["workflow", filters.workflow], ["factory", filters.factory], ["integration", filters.integration], ["repo", filters.repo], ["model", filters.model], ["warningType", filters.warningType], ["failureType", filters.failureType], ["humanTouched", filters.humanTouched], ["mergedPrs", filters.mergedPrs], ["analyticsEnabled", filters.analyticsEnabled], ["requiresPr", filters.requiresPr], ["from", filters.from], ["to", filters.to], ["limit", filters.limit], ["cursor", filters.cursor]]
  for (const [key, value] of entries) if (value !== undefined && value !== null && value !== "") params.set(key, String(value))
  const query = params.toString()
  return query ? `?${query}` : ""
}

export function TicketDetailPanel({ ticket, filters, onClose, onOpenRun }: { ticket: AnalyticsTicket | null; filters?: TaskRunAnalyticsFilters; onClose: () => void; onOpenRun: (run: AnalyticsTicketRunSummary) => void }) {
  const [detail, setDetail] = useState<AnalyticsTicket | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [retry, setRetry] = useState(0)
  const [loadedKey, setLoadedKey] = useState<string | null>(null)
  if ((ticket?.ticketKey ?? null) !== loadedKey) {
    setLoadedKey(ticket?.ticketKey ?? null)
    setDetail(null)
    setError(null)
  }
  const filtersQuery = taskRunAnalyticsQuery(filters)
  useEffect(() => {
    if (!ticket) return
    const controller = new AbortController()
    void fetchAnalyticsTicket(ticket.ticketKey, filters, { signal: controller.signal })
      .then((response) => setDetail(response.ticket))
      .catch(() => { if (!controller.signal.aborted) setError("Failed to load ticket details.") })
    return () => controller.abort()
  }, [ticket?.ticketKey, filtersQuery, retry])
  useEscapeToClose(onClose, Boolean(ticket))
  const panelRef = useRef<HTMLElement>(null)
  useFocusTrap(panelRef, Boolean(ticket), Boolean(detail))
  const previousFocusRef = useRef<HTMLElement | null>(null)
  useEffect(() => {
    if (!ticket) return
    previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    panelRef.current?.focus()
    return () => previousFocusRef.current?.focus()
  }, [ticket?.ticketKey])
  if (!ticket) return null
  if (!detail) return <><div className="fixed inset-0 z-[45] bg-black/50" onClick={onClose} aria-hidden="true" /><aside ref={panelRef} tabIndex={-1} className="fixed inset-y-0 right-0 z-[50] flex w-full max-w-full items-center justify-center border-l bg-background shadow-xl md:max-w-[66vw]" role="dialog" aria-modal="true"><Button variant="ghost" className="absolute right-3 top-3" onClick={onClose}>Close</Button><div className="flex items-center gap-2"><p className="text-sm text-muted-foreground">{error || "Loading ticket details…"}</p>{error && <Button variant="outline" size="sm" onClick={() => { setError(null); setRetry((value) => value + 1) }}>Retry</Button>}</div></aside></>
  const current = detail
  const open = current.prs.filter((pr) => pr.state === "open")
  const delivered = current.status === "delivered"
  const outcome = delivered ? (current.mergedPrCount > 0 ? "Work was delivered through a merged pull request." : "Work was delivered; its pull request was closed after the changes landed.") : current.status === "pr_open" ? `Work is done and waiting on a human: ${open.length} pull request${open.length === 1 ? "" : "s"} open, none merged or closed yet.` : current.status === "in_progress" ? "Work is currently in progress." : "The work did not reach delivery."
  return <>
    <div className="fixed inset-0 z-[45] bg-black/50" onClick={onClose} aria-hidden="true" />
    <aside ref={panelRef} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="ticket-detail-title" className="fixed inset-y-0 right-0 z-[50] flex w-full max-w-full md:max-w-[66vw] flex-col border-l bg-background shadow-xl">
      <header className="flex items-start justify-between gap-3 border-b bg-card p-4"><div className="min-w-0 space-y-1"><div className="flex flex-wrap items-center gap-2"><TicketStatusBadge status={current.status} /><span className={caption}>{current.priority ? `${current.priority} priority` : "No priority"}</span><span className={caption}>via {current.source}</span></div><h2 id="ticket-detail-title" className="text-pretty text-base font-semibold tracking-tight">{current.issueId}: {current.issueTitle}</h2><p className="text-xs text-muted-foreground">{current.requester || "Unknown requester"} · {current.team || "Unassigned"} · reported {date(current.reportedAt)}</p><p className="font-mono text-xs text-muted-foreground">{current.repo || "—"} · {current.workflowName || "—"}</p></div><div className="flex shrink-0 gap-1">{open[0] && <Button asChild variant="outline" size="sm"><a href={open[0].url} target="_blank" rel="noreferrer">Review PR</a></Button>}<Button variant="ghost" size="icon" className="size-8" onClick={onClose} aria-label="Close ticket detail"><X className="size-4" /></Button></div></header>
      <div className="min-h-0 flex-1 space-y-4 overflow-auto p-4">{error && <div className="flex items-center gap-2 rounded-lg border border-destructive/50 bg-destructive/10 px-3 py-2 text-sm text-destructive"><span>{error} Showing stale ticket details.</span><Button variant="outline" size="sm" onClick={() => { setError(null); setRetry((value) => value + 1) }}>Retry</Button></div>}<div className="grid grid-cols-2 gap-2 lg:grid-cols-4">{[["Lead time", duration(current.leadTime), delivered ? (current.mergedPrCount > 0 ? "report → merge" : "report → delivery") : "report → last activity"], ["Time to first run", duration(current.timeToFirstRun), "report → agent started"], ["Total cost", usd.format(current.cost), `${current.runCount} runs · ${current.attemptCount} attempts`], ["Human touches", String(current.humanTouches), current.humanTouches ? "reviews and nudges" : "fully autonomous"]].map(([label, value, sub]) => <div key={label} className="rounded-lg border bg-card p-3"><div className={caption}>{label}</div><div className="mt-1 text-lg font-semibold tabular-nums">{value}</div><div className="text-[11px] text-muted-foreground">{sub}</div></div>)}</div>
        <Section title="What was asked for"><p className="text-sm leading-6">{current.ask || current.issueTitle}</p><Row label="Requested by" value={current.requester || "Unknown"} /><Row label="Team" value={current.team || "Unassigned"} /><Row label="Ticket" value={current.issueId} mono /></Section>
        <Section title="Where it stands" stat={{ left: `${current.runCount} runs · ${current.failedRunCount} failed` }}><p className="text-sm text-muted-foreground">{outcome}</p></Section>
        <Section title="Delivery" stat={{ left: `${current.mergedPrCount} merged · ${current.openPrCount} open` }}>{current.prs.length ? current.prs.map((pr) => { const prState = pr.merged ? "merged" : pr.state; const Icon = prState === "merged" ? GitMerge : prState === "closed" ? XCircle : GitPullRequest; const color = prState === "merged" ? "text-chart-2" : prState === "open" ? "text-chart-1" : "text-muted-foreground"; const prLabel = prState === "merged" ? "Merged" : prState === "open" ? "Open" : prState === "closed" ? "Closed" : prState.toLowerCase(); return <a key={pr.id} href={pr.url} target="_blank" rel="noreferrer" className="flex items-center gap-2 rounded-md border px-3 py-2 text-sm hover:bg-accent"><Icon className={`size-4 ${color}`} /><span className="font-mono">{pr.repo}#{pr.prNumber}</span><span className={`${caption} ${color}`}>{prLabel}</span><span className="ml-auto font-mono text-[11px] text-muted-foreground">{pr.runId}</span></a> }) : <p className="text-sm text-muted-foreground">No pull requests were opened for this ticket.</p>}</Section>
        <Section title="How it went" stat={{ left: `${current.story.length} milestones` }}>{current.story.map((item, index) => { const color = item.kind === "good" ? "var(--chart-2)" : item.kind === "bad" ? "var(--chart-4)" : item.kind === "human" ? "var(--chart-1)" : "var(--muted-foreground)"; return <div key={item.id} className="flex gap-2"><div className="flex w-2 flex-col items-center"><i className="mt-1.5 size-2 rounded-full" style={{ background: color }} />{index < current.story.length - 1 && <i className="w-px flex-1 bg-border" />}</div><div className="min-w-0 flex-1 pb-2"><div className="flex gap-2 text-sm"><span style={item.kind === "neutral" ? undefined : { color }}>{item.label}{item.count > 1 ? ` (${item.count}×)` : ""}</span><span className="ml-auto font-mono text-[11px] text-muted-foreground">{time(item.time)}</span></div><p className="text-xs text-muted-foreground">{item.actor}</p></div></div> })}</Section>
        <Section title="Runs on this ticket" stat={{ left: `${usd.format(current.cost)} total` }}>{current.runs.map((run, index) => { const prs = current.prs.filter((pr) => pr.runId === run.runId); const produced = prs.length ? `produced ${prs.map((pr) => `#${pr.prNumber}`).join(", ")}` : run.status === "running" ? "still working" : "nothing"; return <button type="button" key={run.runId} onClick={() => onOpenRun(run)} className="flex w-full items-center gap-2 rounded-md border px-3 py-2 text-left text-sm hover:bg-accent"><span className={`${caption} w-10`}>Try {index + 1}</span><RunStatusBadge status={run.status} /><span className="min-w-0 flex-1 truncate text-muted-foreground">{produced}</span><span className="font-mono text-xs">{duration(run.lastActivity - run.startedAt)} · {usd.format(run.cost)}</span><ChevronRight className="size-4 text-muted-foreground" /></button> })}<p className="text-[11px] text-muted-foreground">Open a run to see the full detail.</p></Section>
      </div>
    </aside>
  </>
}
