"use client"

import { useState, useCallback, useEffect, useRef } from "react"
import { useRouter } from "next/navigation"
import { Zap, Play, AlertCircle, Info } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog"
import { triggerWorkflow, type WorkflowInput, type Workflow } from "@/lib/api"
import { WorkflowName } from "@/components/workflow-name"

interface ManualTriggerModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  workflow?: Workflow | null
}

export function ManualTriggerModal({ open, onOpenChange, workflow }: ManualTriggerModalProps) {
  const [inputValues, setInputValues] = useState<Record<string, string>>({})
  const [triggering, setTriggering] = useState(false)
  const [triggerError, setTriggerError] = useState<string | null>(null)
  const [signaledNotice, setSignaledNotice] = useState<string | null>(null)
  const [seededKey, setSeededKey] = useState<string | null>(null)
  const [wasOpen, setWasOpen] = useState(open)
  const [feedbackSeen, setFeedbackSeen] = useState(false)
  const router = useRouter()

  // Seed defaults when a different workflow is selected. Done during render
  // with a guarded comparison (React's "adjusting state when props change"
  // pattern) instead of a setState-in-effect, so no stale-values render is
  // committed. Keyed by workflow identity, and ignoring the null the parent
  // passes while closed, so closing and reopening the same workflow does not
  // wipe feedback the user has not seen yet.
  const workflowKey = workflow ? `${workflow.workspaceName}/${workflow.name}` : null
  if (workflow && workflowKey !== seededKey) {
    setSeededKey(workflowKey)
    // Clear on every workflow change — otherwise an error from the previous
    // workflow survives the switch.
    setTriggerError(null)
    setSignaledNotice(null)
    setInputValues(defaultInputValues(workflow))
  }

  // The modal stays mounted between openings, so reset inputs and clear
  // feedback the user already saw each time it reopens (same render-time
  // pattern as above). Closing mid-trigger does not count as seen: that result
  // lands while the modal is closed and must still be shown on the next open.
  if (open !== wasOpen) {
    setWasOpen(open)
    if (!open && !triggering) {
      setFeedbackSeen(true)
    } else if (open) {
      if (workflow) setInputValues(defaultInputValues(workflow))
      if (feedbackSeen) {
        setFeedbackSeen(false)
        setTriggerError(null)
        setSignaledNotice(null)
      }
    }
  }

  // Latest selected workflow, so a trigger result that lands after the user
  // switched to another workflow is not shown against the wrong one.
  const currentKey = useRef(workflowKey)
  useEffect(() => {
    if (workflowKey !== null) currentKey.current = workflowKey
  }, [workflowKey])

  const handleTrigger = useCallback(async () => {
    if (!workflow) return
    const triggeredKey = `${workflow.workspaceName}/${workflow.name}`
    setTriggering(true)
    setTriggerError(null)
    setSignaledNotice(null)
    try {
      const inputs: Record<string, unknown> = {}
      if (workflow.inputs) {
        for (const input of workflow.inputs) {
          const val = inputValues[input.name]
          // Validate required fields
          if (input.required && (val === undefined || val === "")) {
            throw new Error(`Required input "${input.name}" is empty`)
          }
          if (input.type === "bool") {
            inputs[input.name] = val === "true"
          } else if (input.type === "number") {
            if (val !== undefined && val !== "") {
              const num = parseFloat(val)
              if (isNaN(num)) throw new Error(`Invalid number for "${input.name}"`)
              if (input.min !== undefined && num < input.min) {
                throw new Error(`${input.name} must be at least ${input.min}`)
              }
              if (input.max !== undefined && num > input.max) {
                throw new Error(`${input.name} must be at most ${input.max}`)
              }
              inputs[input.name] = num
            }
          } else {
            inputs[input.name] = val || ""
          }
        }
      }
      const result = await triggerWorkflow(workflow, inputs)
      setTriggering(false)
      if (currentKey.current !== triggeredKey) return
      if (result?.status === "signaled") {
        // signal overlap policy: the open run(s) received a cron.tick and no
        // new run was created, so keep the modal open to say so.
        const count = result.target_run_ids?.length ?? 0
        const target = count === 1 ? "the open run" : `${count} open runs`
        setSignaledNotice(`No new run was started. The trigger was sent to ${target}, and the inputs were not applied.`)
        return
      }
      onOpenChange(false)
    } catch (e) {
      setTriggering(false)
      if (currentKey.current !== triggeredKey) return
      // Keep modal open so user sees the backend validation error
      setTriggerError(e instanceof Error ? e.message : String(e))
    }
  }, [workflow, inputValues, onOpenChange])

  if (!workflow) return null

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Zap className="size-4 text-amber-500" />
            Trigger <WorkflowName name={workflow.name} />
          </DialogTitle>
          <DialogDescription>
            {workflow.inputs && workflow.inputs.length > 0
              ? "Fill in the inputs below to trigger this workflow."
              : "Click confirm to trigger this workflow."}
          </DialogDescription>
        </DialogHeader>

        {workflow.inputs && workflow.inputs.length > 0 && (
          <div className="space-y-4 py-2">
            {workflow.inputs.map((input) => (
              <WorkflowInputField
                key={input.name}
                input={input}
                value={inputValues[input.name] || ""}
                onChange={(val) => setInputValues((prev) => ({ ...prev, [input.name]: val }))}
              />
            ))}
          </div>
        )}

        {triggerError && (
          <div className="flex items-center gap-1.5 text-sm text-red-500 bg-red-50 p-2 rounded">
            <AlertCircle className="size-4" />
            <span>{triggerError}</span>
          </div>
        )}

        {signaledNotice && (
          <div className="flex items-center gap-1.5 text-sm text-blue-700 bg-blue-50 p-2 rounded">
            <Info className="size-4 shrink-0" />
            <span>{signaledNotice}</span>
          </div>
        )}

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={triggering}>
            {signaledNotice ? "Close" : "Cancel"}
          </Button>
          <Button onClick={handleTrigger} disabled={triggering}>
            {triggering ? (
              <>
                <Zap className="size-4 mr-1 animate-pulse" />
                Triggering...
              </>
            ) : (
              <>
                <Play className="size-4 mr-1" />
                Confirm
              </>
            )}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function defaultInputValues(workflow: Workflow): Record<string, string> {
  const defaults: Record<string, string> = {}
  for (const input of workflow.inputs ?? []) {
    if (input.default) {
      defaults[input.name] = input.default
    } else if (input.type === "bool") {
      defaults[input.name] = "false"
    } else if (input.type === "enum" && input.options && input.options.length > 0) {
      defaults[input.name] = input.options[0]
    }
  }
  return defaults
}

function WorkflowInputField({
  input,
  value,
  onChange,
}: {
  input: WorkflowInput
  value: string
  onChange: (val: string) => void
}) {
  return (
    <div className="space-y-1.5">
      <label className="text-sm font-medium flex items-center gap-1">
        {input.name}
        {input.required && <span className="text-red-500">*</span>}
        <Badge variant="secondary" className="text-[10px] font-normal">
          {input.type}
        </Badge>
      </label>
      {input.description && (
        <p className="text-xs text-muted-foreground">{input.description}</p>
      )}
      {input.type === "enum" && input.options ? (
        <select
          className="w-full text-sm border border-border rounded-md px-3 py-2 bg-background"
          value={value || input.default || ""}
          onChange={(e) => onChange(e.target.value)}
        >
          {input.options.map((opt) => (
            <option key={opt} value={opt}>{opt}</option>
          ))}
        </select>
      ) : input.type === "bool" ? (
        <select
          className="w-full text-sm border border-border rounded-md px-3 py-2 bg-background"
          value={value || input.default || "false"}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      ) : (
        <Input
          type={input.type === "number" ? "number" : "text"}
          value={value || ""}
          onChange={(e) => onChange(e.target.value)}
          placeholder={input.default}
          min={input.type === "number" ? input.min : undefined}
          max={input.type === "number" ? input.max : undefined}
        />
      )}
    </div>
  )
}
