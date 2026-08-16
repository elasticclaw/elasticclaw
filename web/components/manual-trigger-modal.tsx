"use client"

import { useState, useCallback } from "react"
import { useRouter } from "next/navigation"
import { Zap, Play, AlertCircle } from "lucide-react"
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

interface ManualTriggerModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  workflow?: Workflow | null
}

export function ManualTriggerModal({ open, onOpenChange, workflow }: ManualTriggerModalProps) {
  const [inputValues, setInputValues] = useState<Record<string, string>>({})
  const [triggering, setTriggering] = useState(false)
  const [triggerError, setTriggerError] = useState<string | null>(null)
  const [seededWorkflow, setSeededWorkflow] = useState<Workflow | null | undefined>(undefined)
  const router = useRouter()

  // Seed defaults when workflow changes. Done during render with a guarded
  // comparison (React's "adjusting state when props change" pattern) instead
  // of a setState-in-effect, so no stale-values render is committed.
  if (workflow !== seededWorkflow) {
    setSeededWorkflow(workflow)
    // Clear on every workflow change, not just the has-inputs branch —
    // otherwise an error from the previous workflow survives the switch.
    setTriggerError(null)
    if (!workflow?.inputs) {
      setInputValues({})
    } else {
      const defaults: Record<string, string> = {}
      for (const input of workflow.inputs) {
        if (input.default) {
          defaults[input.name] = input.default
        } else if (input.type === "bool") {
          defaults[input.name] = "false"
        } else if (input.type === "enum" && input.options && input.options.length > 0) {
          defaults[input.name] = input.options[0]
        }
      }
      setInputValues(defaults)
    }
  }

  const handleTrigger = useCallback(async () => {
    if (!workflow) return
    setTriggering(true)
    setTriggerError(null)
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
      await triggerWorkflow(workflow, inputs)
      setTriggering(false)
      onOpenChange(false)
    } catch (e) {
      setTriggering(false)
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
            <Zap className="size-4 text-status-warn" />
            Trigger {workflow.name}
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
          <div className="flex items-center gap-1.5 rounded-md bg-destructive/10 p-2 text-sm text-destructive">
            <AlertCircle className="size-4" />
            <span>{triggerError}</span>
          </div>
        )}

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={triggering}>
            Cancel
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
      <label className="flex items-center gap-1.5 font-mono text-[13px]">
        {input.name}
        {input.required && <span className="text-destructive">*</span>}
        <Badge variant="secondary" className="text-[10px] font-normal">
          {input.type}
        </Badge>
      </label>
      {input.description && (
        <p className="text-xs text-muted-foreground">{input.description}</p>
      )}
      {input.type === "enum" && input.options ? (
        <select
          className="w-full rounded-md border border-input bg-card px-2.5 py-1.5 text-sm outline-none transition-colors hover:border-foreground/45 focus-visible:border-primary"
          value={value || input.default || ""}
          onChange={(e) => onChange(e.target.value)}
        >
          {input.options.map((opt) => (
            <option key={opt} value={opt}>{opt}</option>
          ))}
        </select>
      ) : input.type === "bool" ? (
        <select
          className="w-full rounded-md border border-input bg-card px-2.5 py-1.5 text-sm outline-none transition-colors hover:border-foreground/45 focus-visible:border-primary"
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
