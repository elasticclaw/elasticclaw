"use client"

import { useState } from "react"
import { Check, Copy, ExternalLink, FileCode2, Loader2, RotateCcw, Ticket, TriangleAlert } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatBlock, AIChatTicketRequest } from "@/lib/api"
import { artifactShareURL } from "@/lib/ai-chat-artifacts"
import { CopyArtifactLink, displayURL } from "./artifact-card"

type TicketBlock = Extract<AIChatBlock, { kind: "ticket" }>

export function CopyTicketText({ title, body }: { title: string; body: string }) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const copy = async () => {
    try { await navigator.clipboard.writeText(`${title}\n\n${body}`); setCopied(true); setFailed(false) }
    catch { setFailed(true) }
  }
  return <Button variant="outline" size="sm" onClick={() => void copy()}>{copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{failed ? "Could not copy" : copied ? "Copied" : "Copy as text"}</Button>
}

export function TicketActionCard({ block, onRetry }: { block: TicketBlock; onRetry?: (action: AIChatTicketRequest) => void }) {
  const { card, state } = block
  if (state === "working") return <div role="status" className="my-3 flex items-center gap-3 rounded-xl border bg-card p-3.5">
    <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><Loader2 className="size-4 animate-spin text-muted-foreground" /></span><div className="leading-snug"><div className="font-semibold">Opening ticket in {card.tracker}</div><div className="mt-0.5 text-xs text-muted-foreground">{[card.team, card.state].filter(Boolean).join(" · ")}</div></div>
  </div>
  if (state === "failed") return <div role="alert" className="my-3 flex flex-wrap items-center gap-3 rounded-xl border bg-card p-3.5">
    <div className="flex min-w-0 flex-1 items-center gap-3"><span className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><TriangleAlert className="size-4 text-amber-500" /></span><div className="min-w-0 leading-snug"><div className="font-semibold">{card.outcomeUnknown ? `${card.tracker} did not confirm the ticket` : card.failureHeadline || (card.error === "Stopped. Nothing was created." ? "Ticket creation stopped" : "Could not create the ticket")}</div><div className="mt-0.5 text-[13px] text-muted-foreground">{card.error}</div></div></div>
    <div className="flex shrink-0 flex-wrap gap-2">{onRetry && <Button variant="outline" size="sm" onClick={() => onRetry({ kind: "ticket", actionKey: card.actionKey, title: card.title, body: card.body, artifactId: card.artifactId, ...(card.outcomeUnknown ? { confirmUnknown: card.attemptId || block.id.replace(/-ticket$/, "") } : {}) })}><RotateCcw className="size-3.5" />{card.outcomeUnknown ? `I checked ${card.tracker} — create anyway` : "Try again"}</Button>}{(card.title?.trim() || card.body?.trim()) && <CopyTicketText title={card.title} body={card.body} />}</div>
  </div>
  if (state !== "created" || !card.url) return null
  const artifactURL = card.artifact ? artifactShareURL(card.artifact.slug, card.artifact.shareUrl) : ""
  return <div className="my-3">
    <div className="space-y-3 rounded-xl border bg-card p-3.5">
      <div className="flex items-center justify-between gap-3 text-[11px] text-muted-foreground"><span className="inline-flex items-center gap-1 font-semibold uppercase tracking-wide"><Ticket className="size-3" />{card.tracker} ticket</span>{card.state && <span className="inline-flex items-center gap-1.5"><span className="size-1.5 rounded-full bg-muted-foreground" />{card.state}</span>}</div>
      <div className="flex flex-wrap items-baseline gap-x-2"><span className="font-mono text-xs text-muted-foreground">{card.identifier}</span><span className="font-semibold">{card.title}</span></div>
      <div className="text-[13px] leading-snug text-muted-foreground">{card.summary}</div>
      <div className="flex flex-wrap gap-1.5 text-[11px] text-muted-foreground"><span className="rounded-full border px-2 py-0.5">Team {card.team}</span>{card.labels?.map((label) => <span key={label} className="rounded-full border px-2 py-0.5">{label}</span>)}</div>
      {card.artifact && <a href={artifactURL} target="_blank" rel="noopener noreferrer" className="flex flex-wrap items-center gap-2 rounded-lg border px-3 py-2 !text-foreground !no-underline"><FileCode2 className="size-3.5 shrink-0 text-muted-foreground" /><span className="text-xs font-medium">{card.artifact.title}</span><span className="break-all font-mono text-[11px] text-muted-foreground">{displayURL(artifactURL)}</span></a>}
      <div className="flex flex-wrap gap-2"><Button size="sm" asChild><a href={card.url} target="_blank" rel="noopener noreferrer"><ExternalLink className="size-3.5" />Open in {card.tracker}</a></Button><CopyArtifactLink url={card.url} /></div>
    </div>
    <div className="mt-2 text-sm text-muted-foreground">It is a plain ticket for a person to triage. Nothing else was started.</div>
  </div>
}
