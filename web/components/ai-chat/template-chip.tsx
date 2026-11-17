"use client"

import { useState } from "react"
import { ChevronDown, LayoutTemplate } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatArtifactKit } from "@/lib/api"
import { effectivePin, kitTemplates, pinLabel } from "./artifact-kit"
import { TemplatePicker } from "./template-picker"

export function TemplateChip({ kit, workspace, value, onChange, disabled }: { kit?: AIChatArtifactKit; workspace: string; value: string; onChange: (value: string) => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false)
  if (!kitTemplates(kit).length) return null
  const pin = effectivePin(value, kit)
  const label = pinLabel(pin, kit)
  return <>
    <Button variant="outline" size="sm" className="h-7 gap-1.5 rounded-full px-2.5 text-xs font-normal text-muted-foreground" disabled={disabled} aria-label={`Artifact template: ${label}`} onClick={() => setOpen(true)}><LayoutTemplate className="size-3.5" /><span>Template: <span className={pin ? "text-foreground" : undefined}>{label}</span></span><ChevronDown className="size-3" /></Button>
    <TemplatePicker open={open && !disabled} onOpenChange={setOpen} workspace={workspace} kit={kit} value={pin} includeAuto title="Artifact template" description="Used for pages built in this conversation. Auto lets the assistant choose." onSelect={onChange} />
  </>
}
