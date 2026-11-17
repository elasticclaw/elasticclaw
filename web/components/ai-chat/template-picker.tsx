"use client"

import { useEffect, useRef, useState } from "react"
import { Check, FileCode2, Loader2, Sparkles } from "lucide-react"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { fetchAIChatArtifactKit, type AIChatArtifactKit } from "@/lib/api"
import { getHubUrl } from "@/lib/hub-url"
import { kitTemplates, templatePreviewExpired } from "./artifact-kit"

type PickerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  workspace: string
  kit?: AIChatArtifactKit
  value: string
  includeAuto: boolean
  title: string
  description: string
  onSelect: (value: string) => void
}

export function TemplatePicker({ open, onOpenChange, title, description, ...props }: PickerProps) {
  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="sm:max-w-[720px]">
      <DialogHeader><DialogTitle>{title}</DialogTitle><DialogDescription>{description}</DialogDescription></DialogHeader>
      {open && <TemplateOptions key={props.workspace} {...props} title={title} onSelect={(value) => { props.onSelect(value); onOpenChange(false) }} />}
    </DialogContent>
  </Dialog>
}

// Mount on each open so previews always get fresh capabilities and aborted
// requests cannot replace a later workspace's options.
function TemplateOptions({ workspace, kit, value, includeAuto, title, onSelect }: Omit<PickerProps, "open" | "onOpenChange" | "description">) {
  const [choice, setChoice] = useState(value)
  const [fresh, setFresh] = useState<AIChatArtifactKit>()
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    const controller = new AbortController()
    fetchAIChatArtifactKit(workspace, controller.signal).then(
      (kit) => { if (!controller.signal.aborted) setFresh(kit) },
      () => { if (!controller.signal.aborted) setFailed(true) },
    )
    return () => controller.abort()
  }, [workspace])
  const loading = !fresh && !failed
  // Keep choices aligned with the catalog used to validate the conversation pin.
  const options = [
    ...(includeAuto ? [{ id: "", name: "Auto", description: "Picks a template when the request fits one, otherwise none.", icon: Sparkles }] : []),
    { id: "free", name: "Free", description: "No template. The design system still applies.", icon: FileCode2 },
    ...kitTemplates(kit).map((template) => ({ ...template, icon: undefined })),
  ]
  const selected = options.some((option) => option.id === choice) ? choice : options[0].id
  return <div role="radiogroup" aria-label={title} onKeyDown={(event) => {
    if (!["ArrowRight", "ArrowDown", "ArrowLeft", "ArrowUp", "Home", "End"].includes(event.key)) return
    const buttons = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="radio"]'))
    const index = buttons.indexOf(event.target as HTMLButtonElement)
    if (index < 0) return
    event.preventDefault()
    const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1 : (index + (event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : -1) + buttons.length) % buttons.length
    buttons[next].focus()
    setChoice(options[next].id)
  }} className="grid max-h-[65vh] gap-3 overflow-y-auto p-1 sm:grid-cols-2">
    {options.map((option) => {
      const Icon = option.icon
      const preview = fresh?.templates.find((template) => template.id === option.id)?.previewUrl
      return <button key={option.id} type="button" role="radio" aria-checked={choice === option.id} tabIndex={selected === option.id ? 0 : -1} onClick={() => onSelect(option.id)} className={`rounded-xl border bg-card p-2.5 text-left transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${choice === option.id ? "border-foreground/60 ring-1 ring-foreground/20" : ""}`}>
        {Icon ? <div className="flex aspect-[16/10] items-center justify-center rounded-lg border bg-muted/40"><Icon className="size-8 text-muted-foreground" /></div> : preview && !templatePreviewExpired(preview) ? <TemplateThumbnail src={preview} title={option.name} /> : <div className="flex aspect-[16/10] items-center justify-center rounded-lg border bg-muted/40">{loading ? <Loader2 className="size-5 animate-spin text-muted-foreground" /> : <FileCode2 className="size-8 text-muted-foreground" />}</div>}
        <div className="mt-2 flex items-center justify-between gap-2"><span className="text-sm font-semibold">{option.name}</span>{choice === option.id && <Check className="size-3.5 shrink-0" aria-hidden="true" />}</div>
        <p className="mt-1 line-clamp-2 text-xs text-muted-foreground">{option.description}</p>
      </button>
    })}
  </div>
}

export function TemplateThumbnail({ src, title }: { src: string; title: string }) {
  const wrapper = useRef<HTMLDivElement>(null)
  const [scale, setScale] = useState(0.25)
  useEffect(() => {
    if (!wrapper.current || typeof ResizeObserver === "undefined") return
    const observer = new ResizeObserver(([entry]) => { if (entry) setScale(entry.contentRect.width / 1280) })
    observer.observe(wrapper.current)
    return () => observer.disconnect()
  }, [])
  return <div ref={wrapper} className="relative aspect-[16/10] w-full overflow-hidden rounded-lg border bg-white">
    <iframe src={`${getHubUrl()}${src}`} title={title} sandbox="allow-scripts" loading="lazy" referrerPolicy="no-referrer" tabIndex={-1} aria-hidden="true" className="pointer-events-none absolute left-0 top-0 h-[800px] w-[1280px] origin-top-left border-0" style={{ transform: `scale(${scale})` }} />
  </div>
}
