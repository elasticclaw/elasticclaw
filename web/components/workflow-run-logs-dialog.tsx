"use client"

import { useMemo, useState } from "react"
import { FileTerminal } from "lucide-react"
import { ClawActivityLog } from "@/components/claw-activity-log"
import { fetchActivityMessages, fetchV2WorkflowAttemptLogs } from "@/lib/api"
import { WorkflowName } from "@/components/workflow-name"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import type { WorkflowRun, WorkflowV2Run } from "@/lib/types"

type WorkflowRunLogTarget =
  | { kind: "v1"; run: WorkflowRun }
  | { kind: "v2"; run: WorkflowV2Run }

export function WorkflowRunLogsDialog({ target }: { target: WorkflowRunLogTarget }) {
  const [open, setOpen] = useState(false)
  const disabled = !target.run.claw_id

  const fetcher = useMemo(() => {
    if (target.kind === "v1") {
      const clawId = target.run.claw_id
      if (!clawId) return null
      return {
        fetchInitial: () => fetchActivityMessages(clawId, { limit: 500, order: "desc" }),
        fetchOlder: (before: string) => fetchActivityMessages(clawId, { before, limit: 100, order: "desc" }),
      }
    }
    const runId = target.run.run_id
    const attemptId = target.run.attempt_id
    return {
      fetchInitial: () => fetchV2WorkflowAttemptLogs(runId, attemptId),
      fetchOlder: () => Promise.resolve([]),
    }
  }, [target])

  const runId = target.kind === "v1" ? target.run.id : target.run.run_id
  const { workspace_name: workspaceName, workflow_name: workflowName } = target.run

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
              Agent activity for run {shortId(runId)} of <WorkflowName name={workflowName} /> in {workspaceName}.
            </DialogDescription>
          </DialogHeader>
          <div className="min-h-0 flex-1 px-6 pb-6">
            {fetcher && <ClawActivityLog fetcher={fetcher} />}
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

function shortId(id: string) {
  return id.length > 8 ? id.slice(0, 8) : id
}
