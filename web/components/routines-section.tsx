"use client"

import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react"
import { AlertTriangle, CalendarClock, CheckCircle2, Loader2, Pencil, Play, Plus, Sparkles } from "lucide-react"
import {
  createRoutine,
  draftRoutineWithAI,
  fetchCronWorkflowRuns,
  fetchRoutineNextRun,
  fetchRoutinePreflight,
  fetchWorkspaces,
  triggerRoutine,
  updateWorkflowControls,
  type CreateRoutineInput,
  type RoutinePreflight,
  type Workflow,
} from "@/lib/api"
import type { WorkflowRun } from "@/lib/types"
import { WorkflowRunsDialog } from "@/components/workflow-runs-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"

type RoutineDraft = CreateRoutineInput

const EMPTY_DRAFT: RoutineDraft = {
  name: "",
  task: "",
  schedule: "0 9 * * 1-5",
  timezone: "UTC",
  overlapPolicy: "skip",
  timeout: "2h",
}

export function RoutinesSection({ selectedWorkspace }: { selectedWorkspace: string }) {
  const [workflows, setWorkflows] = useState<Workflow[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [success, setSuccess] = useState("")
  const [creating, setCreating] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [busyRoutine, setBusyRoutine] = useState("")
  const [refreshVersion, setRefreshVersion] = useState(0)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const workspaces = await fetchWorkspaces()
      const workspace = workspaces.find((item) => item.name === selectedWorkspace)
      setWorkflows(workspace?.workflows || [])
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load routines")
    } finally {
      setLoading(false)
    }
  }, [selectedWorkspace])

  useEffect(() => {
    let cancelled = false
    queueMicrotask(() => {
      if (!cancelled) void load()
    })
    return () => { cancelled = true }
  }, [load])

  const routines = useMemo(
    () => workflows.filter((workflow) => Boolean(workflow.schedule)),
    [workflows],
  )

  const patchRoutine = useCallback(async (
    workflow: Workflow,
    patch: {
      enabled?: boolean
      task?: string
      schedule?: string
      timezone?: string
      overlapPolicy?: string
      timeout?: string
    },
  ) => {
    const key = `${workflow.workspaceName}/${workflow.name}`
    setBusyRoutine(key)
    setError("")
    setSuccess("")
    try {
      await updateWorkflowControls(workflow, patch)
      await load()
      setRefreshVersion((value) => value + 1)
      setSuccess(`Updated ${workflow.name}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update routine")
      throw err
    } finally {
      setBusyRoutine("")
    }
  }, [load])

  const runRoutine = useCallback(async (workflow: Workflow) => {
    const key = `${workflow.workspaceName}/${workflow.name}`
    setBusyRoutine(key)
    setError("")
    setSuccess("")
    try {
      await triggerRoutine(workflow.workspaceName, workflow.name)
      setSuccess(`Started ${workflow.name}`)
      setRefreshVersion((value) => value + 1)
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to start routine")
    } finally {
      setBusyRoutine("")
    }
  }, [])

  const addRoutine = useCallback(async (draft: RoutineDraft) => {
    if (workflows.some((workflow) => workflow.name.toLowerCase() === draft.name.toLowerCase())) {
      throw new Error(`A workflow named ${draft.name} already exists in this workspace`)
    }
    setCreating(true)
    setError("")
    setSuccess("")
    try {
      await createRoutine(selectedWorkspace, draft)
      await load()
      setCreateOpen(false)
      setSuccess(`Created ${draft.name}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create routine")
      throw err
    } finally {
      setCreating(false)
    }
  }, [load, selectedWorkspace, workflows])

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold mb-1">Routines</h2>
          <p className="text-sm text-muted-foreground">
            Schedule repeatable agent workflows. The Hub owns the schedule and starts an isolated agent for every run.
          </p>
        </div>
        <Button onClick={() => setCreateOpen(true)} disabled={!selectedWorkspace}>
          <Plus className="size-4" />
          New routine
        </Button>
      </div>

      {error && (
        <div className="rounded-md border border-red-500/20 bg-red-500/10 px-3 py-2 text-sm text-red-500">
          {error}
        </div>
      )}
      {success && (
        <div className="rounded-md border border-emerald-500/20 bg-emerald-500/10 px-3 py-2 text-sm text-emerald-600 dark:text-emerald-400">
          {success}
        </div>
      )}

      {loading ? (
        <div className="flex items-center justify-center gap-2 px-4 py-12 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" />
          Loading routines…
        </div>
      ) : routines.length === 0 ? (
        <div className="rounded-lg border border-dashed px-6 py-12 text-center">
          <CalendarClock className="mx-auto mb-3 size-8 text-muted-foreground" />
          <p className="text-sm font-medium">No routines configured</p>
          <p className="mt-1 text-xs text-muted-foreground">
            Create a scheduled workflow for recurring maintenance, triage, reporting, or repository checks.
          </p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border">
          <div className="hidden grid-cols-[minmax(0,1.2fr)_minmax(190px,1fr)_auto] gap-6 border-b border-border bg-muted/30 px-4 py-2.5 text-[11px] font-medium uppercase tracking-wider text-muted-foreground md:grid">
            <span>Routine</span>
            <span>Schedule &amp; runs</span>
            <span className="min-w-[224px]">Controls</span>
          </div>
          <div className="divide-y divide-border">
          {routines.map((routine) => (
            <RoutineRow
              key={`${routine.workspaceName}/${routine.name}`}
              routine={routine}
              busy={busyRoutine === `${routine.workspaceName}/${routine.name}`}
              refreshVersion={refreshVersion}
              onPatch={patchRoutine}
              onRun={runRoutine}
            />
          ))}
          </div>
        </div>
      )}

      <RoutineEditorDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        title="Create routine"
        description="Create a cron-backed workflow with a task for the agent to execute."
        initialValue={EMPTY_DRAFT}
        saving={creating}
        allowName
        workspaceName={selectedWorkspace}
        onSave={addRoutine}
      />
    </div>
  )
}

function RoutineRow({
  routine,
  busy,
  refreshVersion,
  onPatch,
  onRun,
}: {
  routine: Workflow
  busy: boolean
  refreshVersion: number
  onPatch: (
    workflow: Workflow,
    patch: { enabled?: boolean; task?: string; schedule?: string; timezone?: string; overlapPolicy?: string; timeout?: string },
  ) => Promise<void>
  onRun: (workflow: Workflow) => Promise<void>
}) {
  const [editOpen, setEditOpen] = useState(false)
  const [nextRun, setNextRun] = useState("")
  const [lastRun, setLastRun] = useState<WorkflowRun | null>(null)
  const [preflight, setPreflight] = useState<RoutinePreflight | null>(null)
  const [checking, setChecking] = useState(false)

  const loadStatus = useCallback(async () => {
    setChecking(true)
    const [nextResult, historyResult, preflightResult] = await Promise.allSettled([
      fetchRoutineNextRun(routine.workspaceName, routine.name),
      fetchCronWorkflowRuns(routine.workspaceName, routine.name, 1),
      fetchRoutinePreflight(routine.workspaceName, routine.name),
    ])
    setNextRun(nextResult.status === "fulfilled" ? nextResult.value.next_run : "")
    setLastRun(historyResult.status === "fulfilled" ? historyResult.value.runs?.[0] || null : null)
    setPreflight(preflightResult.status === "fulfilled" ? preflightResult.value : null)
    setChecking(false)
  }, [routine.name, routine.workspaceName])

  useEffect(() => {
    let cancelled = false
    const refresh = async () => {
      await loadStatus()
      if (cancelled) return
    }
    void refresh()
    return () => { cancelled = true }
  }, [loadStatus, refreshVersion, routine.enabled, routine.schedule])

  const blocker = preflight?.checks.find((check) => check.status === "error")

  const editValue: RoutineDraft = {
    name: routine.name,
    task: routine.task || "",
    schedule: routine.schedule || "",
    timezone: routine.timezone || "UTC",
    overlapPolicy: routine.overlapPolicy === "parallel" ? "parallel" : "skip",
    timeout: routine.timeout || "",
  }

  return (
    <div className="px-4 py-4">
      <div className="grid gap-4 md:grid-cols-[minmax(0,1.2fr)_minmax(190px,1fr)_auto] md:items-start md:gap-6">
        <div className="min-w-0">
          <p className="mb-1 text-[10px] font-medium uppercase tracking-wider text-muted-foreground md:hidden">
            Routine
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <p className="min-w-0 truncate text-sm font-medium" title={routine.name}>{routine.name}</p>
            <Badge variant="outline" className="text-[10px]">
              {routine.enabled ? "active" : "paused"}
            </Badge>
            {checking && !preflight ? (
              <Badge variant="outline" className="gap-1 text-[10px] text-muted-foreground">
                <Loader2 className="size-3 animate-spin" />
                checking
              </Badge>
            ) : preflight?.ready ? (
              <Badge className="gap-1 border-emerald-500/30 bg-emerald-500/10 text-[10px] text-emerald-700 dark:text-emerald-300">
                <CheckCircle2 className="size-3" />
                ready
              </Badge>
            ) : preflight ? (
              <Badge className="gap-1 border-amber-500/30 bg-amber-500/10 text-[10px] text-amber-700 dark:text-amber-300">
                <AlertTriangle className="size-3" />
                needs setup
              </Badge>
            ) : null}
            {lastRun && <RunStatusBadge status={lastRun.status} />}
          </div>
          {blocker && (
            <p className="mt-2 text-xs text-amber-700 dark:text-amber-300" title={blocker.description}>
              {blocker.title}
            </p>
          )}
        </div>

        <div className="min-w-0">
          <p className="mb-1 text-[10px] font-medium uppercase tracking-wider text-muted-foreground md:hidden">
            Schedule &amp; runs
          </p>
          <p className="font-mono text-xs text-foreground/80">{routine.schedule}</p>
          <p className="mt-1 text-xs text-muted-foreground">
            {routine.timezone || "UTC"} · {routine.overlapPolicy || "skip"} overlaps
            {routine.timeout ? ` · timeout ${routine.timeout}` : ""}
          </p>
          <div className="mt-2 space-y-0.5 text-xs text-muted-foreground">
            <p>
              <span className="text-foreground/70">Next:</span>{" "}
              {routine.enabled && nextRun ? formatTimestamp(nextRun) : routine.enabled ? "Unavailable" : "Paused"}
            </p>
            <p>
              <span className="text-foreground/70">Last:</span>{" "}
              {lastRun?.started_at ? formatTimestamp(lastRun.started_at) : "No runs yet"}
            </p>
          </div>
        </div>

        <div className="min-w-[224px]">
          <p className="mb-1 text-[10px] font-medium uppercase tracking-wider text-muted-foreground md:hidden">
            Controls
          </p>
          <div className="grid grid-cols-2 gap-2">
          <label className="flex h-9 items-center gap-2 rounded-md border border-border px-2.5 text-xs text-muted-foreground">
            <Switch
              checked={routine.enabled}
              disabled={busy || checking || (!routine.enabled && !preflight?.ready)}
              onCheckedChange={(enabled) => { void onPatch(routine, { enabled }) }}
              aria-label={`Toggle ${routine.name}`}
            />
            Enabled
          </label>
          <Button variant="outline" size="sm" onClick={() => setEditOpen(true)} disabled={busy}>
            <Pencil className="size-3.5" />
            Edit
          </Button>
          <WorkflowRunsDialog workflow={routine} />
          <Button size="sm" onClick={() => { void onRun(routine) }} disabled={busy || checking || !routine.enabled || !preflight?.ready}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Play className="size-3.5" />}
            Run now
          </Button>
          </div>
        </div>
      </div>

      <RoutineEditorDialog
        open={editOpen}
        onOpenChange={setEditOpen}
        title={`Edit ${routine.name}`}
        description="Update the agent task and Hub-owned schedule for this routine."
        initialValue={editValue}
        saving={busy}
        onSave={async (draft) => {
          await onPatch(routine, {
            task: draft.task,
            schedule: draft.schedule,
            timezone: draft.timezone,
            overlapPolicy: draft.overlapPolicy,
            timeout: draft.timeout,
          })
          setEditOpen(false)
        }}
      />
    </div>
  )
}

function RoutineEditorDialog({
  open,
  onOpenChange,
  title,
  description,
  initialValue,
  saving,
  allowName = false,
  workspaceName,
  onSave,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: string
  initialValue: RoutineDraft
  saving: boolean
  allowName?: boolean
  workspaceName?: string
  onSave: (draft: RoutineDraft) => Promise<void>
}) {
  const [draft, setDraft] = useState<RoutineDraft>(initialValue)
  const [aiPrompt, setAIPrompt] = useState("")
  const [drafting, setDrafting] = useState(false)
  const [formError, setFormError] = useState("")

  useEffect(() => {
    if (!open) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setDraft(initialValue)
      setAIPrompt("")
      setDrafting(false)
      setFormError("")
    })
    return () => { cancelled = true }
  }, [initialValue, open])

  const draftWithAI = async () => {
    const description = aiPrompt.trim()
    if (!description) {
      setFormError("Describe the routine you want AI to draft.")
      return
    }
    if (!workspaceName) {
      setFormError("Select a workspace before drafting a routine.")
      return
    }
    setDrafting(true)
    setFormError("")
    try {
      const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
      const result = await draftRoutineWithAI(workspaceName, description, timezone)
      setDraft({
        name: result.name,
        task: result.task,
        schedule: result.schedule,
        timezone: result.timezone || timezone,
        overlapPolicy: result.overlapPolicy || "skip",
        timeout: result.timeout || "2h",
      })
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "Unable to draft routine with AI")
    } finally {
      setDrafting(false)
    }
  }

  const submit = async () => {
    setFormError("")
    if (allowName && !/^[a-z0-9][a-z0-9-]*$/.test(draft.name)) {
      setFormError("Name must use lowercase letters, numbers, and hyphens.")
      return
    }
    if (!draft.task.trim()) {
      setFormError("Describe the task the agent should perform.")
      return
    }
    if (!draft.schedule.trim()) {
      setFormError("A cron schedule is required.")
      return
    }
    try {
      await onSave({
        ...draft,
        name: draft.name.trim(),
        task: draft.task.trim(),
        schedule: draft.schedule.trim(),
        timezone: draft.timezone?.trim() || "UTC",
        timeout: draft.timeout?.trim(),
      })
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "Unable to save routine")
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-2">
          {allowName && (
            <div className="space-y-2 rounded-lg border border-border bg-muted/20 p-3">
              <div>
                <p className="text-sm font-medium">Draft with AI</p>
                <p className="text-xs text-muted-foreground">
                  Describe the outcome and timing. AI fills the form for you to review; it does not save the routine.
                </p>
              </div>
              <Textarea
                value={aiPrompt}
                onChange={(event) => setAIPrompt(event.target.value)}
                placeholder="Every weekday at 9 AM, inspect dependency health, apply safe updates, run tests, and open a pull request."
                className="min-h-20"
                disabled={drafting || saving}
              />
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => { void draftWithAI() }}
                disabled={drafting || saving || !aiPrompt.trim()}
              >
                {drafting ? <Loader2 className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
                {drafting ? "Drafting…" : "Draft with AI"}
              </Button>
            </div>
          )}
          {allowName && (
            <Field label="Name" hint="Lowercase letters, numbers, and hyphens.">
              <Input
                value={draft.name}
                onChange={(event) => setDraft((value) => ({ ...value, name: event.target.value }))}
                placeholder="dependency-health"
              />
            </Field>
          )}
          <Field label="Agent task" hint="The instruction injected when each run starts.">
            <Textarea
              value={draft.task}
              onChange={(event) => setDraft((value) => ({ ...value, task: event.target.value }))}
              placeholder="Inspect dependency health, apply safe updates, run tests, and open a pull request."
              className="min-h-28"
            />
          </Field>
          <Field label="Cron schedule" hint='Five-field cron, for example "0 9 * * 1-5".'>
            <Input
              value={draft.schedule}
              onChange={(event) => setDraft((value) => ({ ...value, schedule: event.target.value }))}
              className="font-mono"
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Timezone" hint="IANA timezone name.">
              <Input
                value={draft.timezone}
                onChange={(event) => setDraft((value) => ({ ...value, timezone: event.target.value }))}
                placeholder="UTC"
              />
            </Field>
            <Field label="Timeout" hint="Go duration, such as 30m or 2h.">
              <Input
                value={draft.timeout}
                onChange={(event) => setDraft((value) => ({ ...value, timeout: event.target.value }))}
                placeholder="2h"
              />
            </Field>
          </div>
          <Field label="When a previous run is active" hint="Queueing is not available in this version.">
            <Select
              value={draft.overlapPolicy}
              onValueChange={(overlapPolicy: "skip" | "parallel") => setDraft((value) => ({ ...value, overlapPolicy }))}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="skip">Skip the new run</SelectItem>
                <SelectItem value="parallel">Start another run</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          {formError && <p className="text-sm text-red-500">{formError}</p>}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={saving || drafting}>Cancel</Button>
          <Button onClick={() => { void submit() }} disabled={saving || drafting}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            {allowName ? "Save paused" : "Save changes"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Field({ label, hint, children }: { label: string; hint: string; children: ReactNode }) {
  return (
    <label className="block space-y-1.5">
      <span className="text-sm font-medium">{label}</span>
      {children}
      <span className="block text-xs text-muted-foreground">{hint}</span>
    </label>
  )
}

function RunStatusBadge({ status }: { status: string }) {
  const normalized = status.toLowerCase()
  const className = normalized === "completed"
    ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
    : normalized === "failed"
      ? "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-300"
      : normalized === "running"
        ? "border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-300"
        : "border-border bg-muted text-muted-foreground"
  return <Badge className={`text-[10px] ${className}`}>{status}</Badge>
}

function formatTimestamp(value: string) {
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString()
}
