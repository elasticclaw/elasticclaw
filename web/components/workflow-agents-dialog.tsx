"use client"

import { useRef, useState } from "react"
import { Users } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { AgentConfigForm } from "@/components/agent-config-form"
import { updateWorkflowControls, type Workflow } from "@/lib/api"
import { validateAgentConfig, type AgentConfig } from "@/lib/agent-config"

export function WorkflowAgentsDialog({
  workflow,
  onSaved,
  disabled,
}: {
  workflow: Workflow
  onSaved: (workflow: Workflow) => void
  disabled?: boolean
}) {
  const [open, setOpen] = useState(false)
  const openerRef = useRef<HTMLButtonElement>(null)

  return (
    <>
      <Button ref={openerRef} variant="ghost" size="sm" disabled={disabled} onClick={() => setOpen(true)}>
        <Users className="size-3.5" />
        Agents
      </Button>
      {open && (
        <WorkflowAgentsEditor
          key={`${workflow.workspaceName}/${workflow.name}`}
          workflow={workflow}
          onClose={() => setOpen(false)}
          onRestoreFocus={() => openerRef.current?.focus()}
          onSaved={onSaved}
        />
      )}
    </>
  )
}

function WorkflowAgentsEditor({
  workflow,
  onClose,
  onRestoreFocus,
  onSaved,
}: {
  workflow: Workflow
  onClose: () => void
  onRestoreFocus: () => void
  onSaved: (workflow: Workflow) => void
}) {
  const [value, setValue] = useState<AgentConfig>(() => structuredClone(workflow.agents ?? {}))
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")

  async function save() {
    const validation = validateAgentConfig(value)
    if (validation) {
      setError(validation)
      return
    }
    setSaving(true)
    setError("")
    try {
      const updated = await updateWorkflowControls(workflow, { agents: value })
      onSaved(updated)
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not save agent settings")
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={open => { if (!open && !saving) onClose() }}>
      <DialogContent
        className="max-h-[85vh] overflow-y-auto sm:max-w-lg"
        onCloseAutoFocus={event => {
          event.preventDefault()
          onRestoreFocus()
        }}
      >
        <DialogHeader>
          <DialogTitle>Agents · {workflow.name}</DialogTitle>
          <DialogDescription>
            Choose who coordinates this workflow and who handles delegated tasks.
            Changes apply to future runs.
          </DialogDescription>
        </DialogHeader>
        <AgentConfigForm value={value} onChange={setValue} disabled={saving} />
        {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="outline" disabled={saving} onClick={onClose}>Cancel</Button>
          <Button disabled={saving} onClick={save}>
            {saving ? "Saving…" : "Save agents"}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
