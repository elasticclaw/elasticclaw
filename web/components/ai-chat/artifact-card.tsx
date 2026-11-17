"use client"

import { useState } from "react"
import { Check, Copy, ExternalLink, FileCode2, LayoutTemplate, Loader2, RotateCcw, TriangleAlert, Ticket, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatArtifactKit, AIChatArtifactRequest, AIChatBlock, AIChatTicketRequest, AIChatTicketTarget } from "@/lib/api"
import { artifactShareURL } from "@/lib/ai-chat-artifacts"
import { kitTemplates, templateLabel } from "./artifact-kit"
import { TemplatePicker } from "./template-picker"

type ActionBlock = Extract<AIChatBlock, { type: "action" }>

// Links keep the full URL; the visible text drops the scheme, as in lite-03.
export function displayURL(url: string) {
  return url.replace(/^https?:\/\//, "")
}

export function CopyArtifactLink({ url, className }: { url: string; className?: string }) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const copy = async () => {
    try { await navigator.clipboard.writeText(url); setCopied(true); setFailed(false) }
    catch { setFailed(true) }
  }
  return <Button variant="outline" size="sm" className={className} onClick={() => void copy()}>{copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{failed ? "Could not copy" : copied ? "Copied" : "Copy link"}</Button>
}

// `currentTemplate` is the template of the artifact's latest version, which a
// template change revises; older cards may show a different one.
export function ArtifactActionCard({ kit, workspace = "", currentTemplate, onChangeTemplate, block, onStop, onRetry, ticket, onTicket }: { kit?: AIChatArtifactKit; workspace?: string; currentTemplate?: string; onChangeTemplate?: (artifactId: string, title: string, template: string) => void; block: ActionBlock; ticket?: AIChatTicketTarget; onTicket?: (action: AIChatTicketRequest) => void; onStop?: () => void; onRetry?: (action: AIChatArtifactRequest) => void }) {
  const [pickerOpen, setPickerOpen] = useState(false)
  if (block.kind !== "artifact") return null
  const { card, state } = block
  if (card.deleted) return <div className="my-3 rounded-xl border bg-muted/30 p-3.5 text-sm text-muted-foreground">This artifact was deleted.</div>
  if (state === "working") return <div role="status" className="my-3 flex items-center gap-4 rounded-xl border bg-card p-3.5">
    <div className="flex h-24 w-36 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><Loader2 className="size-5 animate-spin text-muted-foreground" /></div>
    <div className="min-w-0 flex-1"><div className="font-semibold">Building “{card.title}”</div><div className="mt-1 text-xs text-muted-foreground">Writing HTML · {card.lines ?? 0} lines so far{card.templateName && ` · ${card.templateName} template`}</div><div className="mt-3 h-1 overflow-hidden rounded bg-muted"><div className="h-full w-2/3 animate-pulse bg-foreground/70" /></div></div>
    {onStop && <Button variant="outline" size="sm" onClick={onStop}><X className="size-3.5" />Stop</Button>}
  </div>
  if (state === "failed") return <div className="my-3"><div role="alert" className="flex items-center gap-3 rounded-xl border bg-card p-3.5">
    <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><TriangleAlert className="size-4 text-amber-500" /></div><div className="min-w-0 flex-1 leading-snug"><div className="font-semibold">Could not save the artifact</div><div className="mt-0.5 text-[13px] text-muted-foreground">{card.error}</div></div>
    {onRetry && <Button variant="outline" size="sm" onClick={() => onRetry({ kind: "artifact", artifactId: card.artifactId, title: card.title, brief: card.brief, ...(card.template ? { template: card.template } : {}) })}><RotateCcw className="size-3.5" />Try again</Button>}
  </div>{renderNotices(card.notices)}</div>
  if (state !== "created" || !card.slug) return null
  const url = artifactShareURL(card.slug, card.shareUrl)
  const label = templateLabel(card, kit)
  const canChangeTemplate = onChangeTemplate && card.artifactId && kitTemplates(kit).length > 0
  const latestTemplate = (currentTemplate ?? card.template) || "free"
  return <div className="my-3">
    <div className="flex items-center gap-4 rounded-xl border bg-card p-3.5">
      <div aria-hidden="true" className="flex h-24 w-36 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><FileCode2 className="size-8 text-muted-foreground" /></div>
      <div className="min-w-0 flex-1"><div className="flex items-center gap-2 text-[11px] text-muted-foreground"><span className="inline-flex items-center gap-1 font-semibold tracking-wide"><FileCode2 className="size-3" />ARTIFACT</span><span>v{card.version} · {Math.max(1, Math.round((card.bytes ?? 0) / 1024))} KB</span>{label && <span className="inline-flex items-center gap-1">· <LayoutTemplate className="size-3" />{label}</span>}</div>
        <div className="mt-1 font-semibold">{card.title}</div><div className="my-2 break-all font-mono text-xs text-muted-foreground">{displayURL(url)}</div>
        <div className="flex flex-wrap gap-2"><Button size="sm" asChild><a href={url} target="_blank" rel="noreferrer"><ExternalLink className="size-3.5" />Open in new tab</a></Button><CopyArtifactLink url={url} />{canChangeTemplate && <Button variant="outline" size="sm" onClick={() => setPickerOpen(true)}><LayoutTemplate className="size-3.5" />Change template</Button>}</div>
      </div>
    </div>
    {renderNotices(card.notices)}
    {canChangeTemplate && <TemplatePicker open={pickerOpen} onOpenChange={setPickerOpen} workspace={workspace} kit={kit} value={latestTemplate} includeAuto={false} title="Change template" description="Publishes a new version with the same content in the chosen template." onSelect={(template) => { if (template !== latestTemplate) onChangeTemplate(card.artifactId!, card.title, template) }} />}
    <div className="mt-2 text-sm text-muted-foreground">Ask for changes and I will publish a new version at the same link.</div>
    {ticket && onTicket && card.artifactId && <Button variant="outline" size="sm" className="mt-2" onClick={() => onTicket({ kind: "ticket", artifactId: card.artifactId })}><Ticket className="size-3.5" />{ticket.verb} with this artifact</Button>}
  </div>
}

function renderNotices(notices?: string[]) {
  return notices?.map((notice, index) => <p key={index} className="mt-2 flex items-start gap-1.5 text-xs text-amber-600 dark:text-amber-500"><TriangleAlert className="mt-0.5 size-3 shrink-0" />{notice}</p>)
}
