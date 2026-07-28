"use client"

import { useState } from "react"
import { FileTerminal } from "lucide-react"
import { ClawActivityLog } from "@/components/claw-activity-log"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import type { WorkflowRun } from "@/lib/types"

export function WorkflowRunLogsDialog({ run }: { run: WorkflowRun }) {
  const [open, setOpen] = useState(false)
  const disabled = !run.claw_id

  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex" tabIndex={disabled ? 0 : undefined}>
            <Button variant="outline" size="sm" disabled={disabled} onClick={() => setOpen(true)}>
              <FileTerminal className="size-4" />
              Agent logs
            </Button>
          </span>
        </TooltipTrigger>
        {disabled && <TooltipContent>This run is not linked to an agent, so logs are unavailable.</TooltipContent>}
      </Tooltip>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="flex h-[min(85vh,800px)] flex-col gap-3 overflow-hidden p-0 sm:max-w-5xl">
          <DialogHeader className="shrink-0 border-b px-6 py-5 pr-12">
            <DialogTitle>Agent logs</DialogTitle>
            <DialogDescription>
              Agent activity for run {shortId(run.id)} of {run.workflow_name} in {run.workspace_name}.
            </DialogDescription>
          </DialogHeader>
          <div className="min-h-0 flex-1 px-6 pb-6">
            {run.claw_id && <ClawActivityLog clawId={run.claw_id} />}
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

function shortId(id: string) {
  return id.length > 8 ? id.slice(0, 8) : id
}
