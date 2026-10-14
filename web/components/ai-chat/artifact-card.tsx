"use client"

import { useState } from "react"
import { Check, Copy, ExternalLink, FileCode2, Loader2, RotateCcw, TriangleAlert, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatArtifactRequest, AIChatBlock } from "@/lib/api"
import { artifactShareURL } from "@/lib/ai-chat-artifacts"

type ActionBlock = Extract<AIChatBlock, { type: "action" }>

export function CopyArtifactLink({ url }: { url: string }) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const copy = async () => {
    try { await navigator.clipboard.writeText(url); setCopied(true); setFailed(false) }
    catch { setFailed(true) }
  }
  return <Button variant="outline" size="sm" onClick={() => void copy()}>{copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{failed ? "Could not copy" : copied ? "Copied" : "Copy link"}</Button>
}

export function ArtifactActionCard({ block, onStop, onRetry }: { block: ActionBlock; onStop?: () => void; onRetry?: (action: AIChatArtifactRequest) => void }) {
  if (block.kind !== "artifact") return null
  const { card, state } = block
  if (state === "working") return <div role="status" className="my-3 flex items-center gap-4 rounded-xl border bg-card p-3.5">
    <div className="flex h-24 w-36 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><Loader2 className="size-5 animate-spin text-muted-foreground" /></div>
    <div className="min-w-0 flex-1"><div className="font-semibold">Building “{card.title}”</div><div className="mt-1 text-xs text-muted-foreground">Writing HTML · {card.lines ?? 0} lines so far</div><div className="mt-3 h-1 overflow-hidden rounded bg-muted"><div className="h-full w-2/3 animate-pulse bg-foreground/70" /></div></div>
    {onStop && <Button variant="outline" size="sm" onClick={onStop}><X className="size-3.5" />Stop</Button>}
  </div>
  if (state === "failed") return <div role="alert" className="my-3 flex items-center gap-3 rounded-xl border bg-card p-3.5">
    <TriangleAlert className="size-5 shrink-0 text-amber-500" /><div className="min-w-0 flex-1"><div className="font-semibold">Could not save the artifact</div><p className="mt-1 text-xs text-muted-foreground">{card.error}</p></div>
    {onRetry && <Button variant="outline" size="sm" onClick={() => onRetry({ kind: "artifact", artifactId: card.artifactId, title: card.title, brief: card.brief })}><RotateCcw className="size-3.5" />Try again</Button>}
  </div>
  if (state !== "created" || !card.slug) return null
  const url = artifactShareURL(card.slug, card.shareUrl)
  return <div className="my-3">
    <div className="flex items-center gap-4 rounded-xl border bg-card p-3.5">
      <div aria-hidden="true" className="flex h-24 w-36 shrink-0 items-center justify-center rounded-lg border bg-muted/40"><FileCode2 className="size-8 text-muted-foreground" /></div>
      <div className="min-w-0 flex-1"><div className="flex items-center gap-2 text-[11px] text-muted-foreground"><span className="inline-flex items-center gap-1 font-semibold tracking-wide"><FileCode2 className="size-3" />ARTIFACT</span><span>v{card.version} · {Math.max(1, Math.round((card.bytes ?? 0) / 1024))} KB</span></div>
        <div className="mt-1 font-semibold">{card.title}</div><div className="my-2 break-all font-mono text-xs text-muted-foreground">{url}</div>
        <div className="flex flex-wrap gap-2"><Button size="sm" asChild><a href={url} target="_blank" rel="noreferrer"><ExternalLink className="size-3.5" />Open in new tab</a></Button><CopyArtifactLink url={url} /></div>
      </div>
    </div>
    <p className="mt-2 text-xs text-muted-foreground">Ask for changes and I will publish a new version at the same link.</p>
  </div>
}
