"use client"

import Link from "next/link"
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ElementType,
  type KeyboardEvent,
  type ReactNode,
} from "react"
import {
  AlertTriangle,
  Check,
  CheckCircle2,
  ClipboardList,
  Copy,
  GitBranch,
  Info,
  LayoutTemplate,
  Loader2,
  Lock,
  Play,
  Settings,
  ShieldCheck,
  Workflow,
  XCircle,
} from "lucide-react"

import {
  fetchWorkflowSetupContext,
  fetchWorkflowSetupPatterns,
  renderWorkflowSetup,
  saveWorkflowSetup,
  triggerWorkflow,
  validateWorkflowSetup,
  type Diagnostic,
  type PatternMetadata,
  type RenderResponse,
  type SaveResponse,
  type SetupContext,
  type SetupIssueTrackerRef,
  type ValidateResponse,
  type Workflow as SavedWorkflow,
  type WorkflowInput,
  type Workspace,
} from "@/lib/api"
import { cn } from "@/lib/utils"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"

interface WorkflowSetupDraft {
  workspaceName: string
  workflowName: string
  patternId: string
  repository: string
  triggerLabel: string
  enableManualTrigger?: boolean
  trackerWorkspace: string
  triggerStatus: string
  workingStatus: string
  prOpenedStatus: string
  mergedStatus: string
  closedNoMergeStatus: string
  manualInputName: string
  manualInputType: string
  manualInputDescription: string
  manualInputDefault: string
  manualInputRequired?: boolean
  concurrencyGroup: string
  includePreCommit?: boolean
  preCommitCommand: string
  doneSignal: string
}

interface WorkflowSetupShellProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onChangeWorkspace: () => void
  onWorkflowSaved?: (response: SaveResponse) => void
  workspaceName: string
  workspaces: Workspace[]
}

type StepId = "pattern" | "access" | "trigger" | "lifecycle" | "review"
type RequirementStatus = "available" | "missing" | "not_used"

interface StepDefinition {
  id: StepId
  label: string
  icon: ElementType
}

interface AccessRequirement {
  id: string
  label: string
  description: string
  detail: string
  status: RequirementStatus
  settingsHref?: string
  settingsLabel?: string
}

interface RenderInput {
  workflowName: string
  patternId: string
  config: Record<string, unknown>
}

interface ManualInputConfig {
  name?: string
  type?: string
  required?: boolean
  default?: string
  description?: string
}

interface DraftFieldErrors {
  workflowName?: string
  repository?: string
  trackerWorkspace?: string
  manualInputName?: string
}

const DRAFT_STORAGE_PREFIX = "workflow-setup-draft"
const SETTINGS_SECTIONS = {
  runtimes: "/settings/runtimes",
  models: "/settings/models",
  secrets: "/settings/secrets",
  issueTrackers: "/settings/issue-trackers",
  github: "/settings/github",
} as const

const STEPS: StepDefinition[] = [
  { id: "pattern", label: "Pattern", icon: LayoutTemplate },
  { id: "access", label: "Access", icon: ShieldCheck },
  { id: "trigger", label: "Trigger", icon: Play },
  { id: "lifecycle", label: "Lifecycle", icon: Workflow },
  { id: "review", label: "Review", icon: ClipboardList },
]

const DIAGNOSTIC_RANK: Record<Diagnostic["severity"], number> = {
  critical: 0,
  warning: 1,
  info: 2,
}

function createDraft(workspaceName: string): WorkflowSetupDraft {
  return {
    workspaceName,
    workflowName: "",
    patternId: "",
    repository: "",
    triggerLabel: "",
    trackerWorkspace: "",
    triggerStatus: "",
    workingStatus: "",
    prOpenedStatus: "",
    mergedStatus: "",
    closedNoMergeStatus: "",
    manualInputName: "",
    manualInputType: "",
    manualInputDescription: "",
    manualInputDefault: "",
    concurrencyGroup: "",
    preCommitCommand: "",
    doneSignal: "",
  }
}

function readDraft(workspaceName: string): WorkflowSetupDraft {
  if (typeof window === "undefined") return createDraft(workspaceName)

  try {
    const raw = window.sessionStorage.getItem(`${DRAFT_STORAGE_PREFIX}:${workspaceName}`)
    if (!raw) return createDraft(workspaceName)
    const parsed = JSON.parse(raw) as Partial<WorkflowSetupDraft>
    return {
      ...createDraft(workspaceName),
      workflowName: stringValue(parsed.workflowName),
      patternId: stringValue(parsed.patternId),
      repository: stringValue(parsed.repository),
      triggerLabel: stringValue(parsed.triggerLabel),
      enableManualTrigger: optionalBooleanValue(parsed.enableManualTrigger),
      trackerWorkspace: stringValue(parsed.trackerWorkspace),
      triggerStatus: stringValue(parsed.triggerStatus),
      workingStatus: stringValue(parsed.workingStatus),
      prOpenedStatus: stringValue(parsed.prOpenedStatus),
      mergedStatus: stringValue(parsed.mergedStatus),
      closedNoMergeStatus: stringValue(parsed.closedNoMergeStatus),
      manualInputName: stringValue(parsed.manualInputName),
      manualInputType: stringValue(parsed.manualInputType),
      manualInputDescription: stringValue(parsed.manualInputDescription),
      manualInputDefault: stringValue(parsed.manualInputDefault),
      manualInputRequired: optionalBooleanValue(parsed.manualInputRequired),
      concurrencyGroup: stringValue(parsed.concurrencyGroup),
      includePreCommit: optionalBooleanValue(parsed.includePreCommit),
      preCommitCommand: stringValue(parsed.preCommitCommand),
      doneSignal: stringValue(parsed.doneSignal),
    }
  } catch {
    return createDraft(workspaceName)
  }
}

function writeDraft(draft: WorkflowSetupDraft): void {
  if (typeof window === "undefined" || !draft.workspaceName) return

  window.sessionStorage.setItem(
    `${DRAFT_STORAGE_PREFIX}:${draft.workspaceName}`,
    JSON.stringify({
      workflowName: draft.workflowName,
      patternId: draft.patternId,
      repository: draft.repository,
      triggerLabel: draft.triggerLabel,
      enableManualTrigger: draft.enableManualTrigger,
      trackerWorkspace: draft.trackerWorkspace,
      triggerStatus: draft.triggerStatus,
      workingStatus: draft.workingStatus,
      prOpenedStatus: draft.prOpenedStatus,
      mergedStatus: draft.mergedStatus,
      closedNoMergeStatus: draft.closedNoMergeStatus,
      manualInputName: draft.manualInputName,
      manualInputType: draft.manualInputType,
      manualInputDescription: draft.manualInputDescription,
      manualInputDefault: draft.manualInputDefault,
      manualInputRequired: draft.manualInputRequired,
      concurrencyGroup: draft.concurrencyGroup,
      includePreCommit: draft.includePreCommit,
      preCommitCommand: draft.preCommitCommand,
      doneSignal: draft.doneSignal,
    })
  )
}

export function WorkflowSetupShell({
  open,
  onOpenChange,
  onChangeWorkspace,
  onWorkflowSaved,
  workspaceName,
  workspaces,
}: WorkflowSetupShellProps) {
  return (
    <WorkflowSetupShellInner
      key={workspaceName}
      open={open}
      onOpenChange={onOpenChange}
      onChangeWorkspace={onChangeWorkspace}
      onWorkflowSaved={onWorkflowSaved}
      workspaceName={workspaceName}
      workspaces={workspaces}
    />
  )
}

function WorkflowSetupShellInner({
  open,
  onOpenChange,
  onChangeWorkspace,
  onWorkflowSaved,
  workspaceName,
  workspaces,
}: WorkflowSetupShellProps) {
  const [draft, setDraft] = useState<WorkflowSetupDraft>(() => readDraft(workspaceName))
  const [activeStep, setActiveStep] = useState<StepId>("pattern")
  const [patterns, setPatterns] = useState<PatternMetadata[]>([])
  const [patternsLoaded, setPatternsLoaded] = useState(false)
  const [patternsError, setPatternsError] = useState("")
  const [setupContext, setSetupContext] = useState<SetupContext | null>(null)
  const [contextLoaded, setContextLoaded] = useState(false)
  const [contextError, setContextError] = useState("")
  const [renderResponse, setRenderResponse] = useState<RenderResponse | null>(null)
  const [renderPending, setRenderPending] = useState(false)
  const [renderError, setRenderError] = useState("")
  const [validateResponse, setValidateResponse] = useState<ValidateResponse | null>(null)
  const [validatePending, setValidatePending] = useState(false)
  const [validateError, setValidateError] = useState("")
  const [validatedConfigHash, setValidatedConfigHash] = useState("")
  const [copiedConfigHash, setCopiedConfigHash] = useState("")
  const [attemptedSave, setAttemptedSave] = useState(false)
  const [warningSaveConfirmed, setWarningSaveConfirmed] = useState(false)
  const [savePending, setSavePending] = useState(false)
  const [saveError, setSaveError] = useState("")
  const [savedResponse, setSavedResponse] = useState<SaveResponse | null>(null)
  const renderSequence = useRef(0)
  const stepButtonRefs = useRef<Partial<Record<StepId, HTMLButtonElement | null>>>({})

  useEffect(() => {
    if (draft.workspaceName !== workspaceName) return
    writeDraft(draft)
  }, [draft, workspaceName])

  useEffect(() => {
    if (!open) return

    let active = true

    fetchWorkflowSetupPatterns()
      .then((data) => {
        if (!active) return
        setPatterns(data)
        setPatternsError("")
        setPatternsLoaded(true)
        setDraft((current) => {
          if (data.length === 0 || data.some((pattern) => pattern.id === current.patternId)) return current
          return { ...current, patternId: data[0].id }
        })
      })
      .catch((error) => {
        if (active) {
          setPatterns([])
          setPatternsError(error instanceof Error ? error.message : "Failed to load workflow patterns")
          setPatternsLoaded(true)
        }
      })

    return () => {
      active = false
    }
  }, [open])

  useEffect(() => {
    if (!open || !workspaceName) return

    let active = true

    fetchWorkflowSetupContext(workspaceName)
      .then((data) => {
        if (!active) return
        setSetupContext(data)
        setContextError("")
        setContextLoaded(true)
      })
      .catch((error) => {
        if (active) {
          setSetupContext(null)
          setContextError(error instanceof Error ? error.message : "Failed to load workspace setup context")
          setContextLoaded(true)
        }
      })

    return () => {
      active = false
    }
  }, [open, workspaceName])

  const selectedWorkspace = useMemo(
    () => workspaces.find((workspace) => workspace.name === workspaceName),
    [workspaceName, workspaces]
  )
  const selectedPattern = useMemo(
    () => patterns.find((pattern) => pattern.id === draft.patternId),
    [draft.patternId, patterns]
  )
  const contextReady = setupContext?.workspace.name === workspaceName
  const patternsLoading = open && !patternsLoaded && patterns.length === 0 && !patternsError
  const contextLoading = open && !contextLoaded && Boolean(workspaceName) && !contextReady && !contextError
  const workflowName = draft.workflowName.trim()
  const renderInput = useMemo(
    () => (selectedPattern ? buildRenderInput(selectedPattern, draft) : null),
    [draft, selectedPattern]
  )
  const renderReadiness = useMemo(
    () => getRenderReadiness(selectedPattern, draft, renderInput),
    [draft, renderInput, selectedPattern]
  )
  const accessRequirements = useMemo(
    () => buildAccessRequirements(selectedPattern, draft, setupContext, contextReady),
    [contextReady, draft, selectedPattern, setupContext]
  )
  const diagnostics = useMemo(
    () => sortDiagnostics([...(renderResponse?.warnings ?? []), ...(validateResponse?.checks ?? [])]),
    [renderResponse, validateResponse]
  )
  const diagnosticGroups = useMemo(() => groupDiagnostics(diagnostics), [diagnostics])
  const hasWarnings = diagnostics.some((diagnostic) => diagnostic.severity === "warning")
  const hasCriticals = diagnostics.some((diagnostic) => diagnostic.severity === "critical")
  const currentConfigHash = renderResponse?.configHash ?? ""
  const copiedPreview = Boolean(currentConfigHash) && copiedConfigHash === currentConfigHash
  const validationMatchesCurrent =
    Boolean(currentConfigHash) &&
    Boolean(validatedConfigHash) &&
    currentConfigHash === validatedConfigHash
  const draftFieldErrors = useMemo(
    () => (attemptedSave ? getDraftFieldErrors(selectedPattern, draft, renderInput) : {}),
    [attemptedSave, draft, renderInput, selectedPattern]
  )
  const saveBlockReason = getSaveBlockReason({
    renderPending,
    validatePending,
    renderResponse,
    renderReadiness,
    validationMatchesCurrent,
    hasCriticals,
  })
  const saveReady = !saveBlockReason
  const canSaveWorkflow = saveReady && !hasWarnings && !savePending
  const canSaveWithWarnings = saveReady && hasWarnings && warningSaveConfirmed && !savePending
  const footerStatus = getFooterStatus({
    hasWarnings,
    saveBlockReason,
    saveError,
    savePending,
    savedResponse,
    warningSaveConfirmed,
  })
  const liveStatus = getLiveStatus({
    footerStatus,
    renderPending,
    validatePending,
    renderError,
    validateError,
    savedResponse,
    savePending,
  })

  useEffect(() => {
    let active = true
    renderSequence.current += 1
    const sequence = renderSequence.current

    const clearRenderState = () => {
      setRenderPending(false)
      setValidatePending(false)
      setRenderResponse(null)
      setRenderError("")
      setValidateResponse(null)
      setValidateError("")
      setValidatedConfigHash("")
    }

    if (!open || !selectedPattern || !renderInput || !renderReadiness.ready) {
      queueMicrotask(() => {
        if (active && sequence === renderSequence.current) clearRenderState()
      })
      return () => {
        active = false
      }
    }

    queueMicrotask(() => {
      if (!active || sequence !== renderSequence.current) return
      setRenderPending(true)
      setValidatePending(false)
      setRenderError("")
      setValidateError("")
      setRenderResponse(null)
      setValidateResponse(null)
      setValidatedConfigHash("")
    })

    renderWorkflowSetup(renderInput)
      .then((rendered) => {
        if (!active || sequence !== renderSequence.current) return
        setRenderResponse(rendered)
        setRenderPending(false)
        setValidatePending(true)
        validateWorkflowSetup({
          workflowName: rendered.workflowName,
          config: rendered.config,
          workspace: workspaceName,
          workspaceConfig: selectedWorkspace?.config,
        })
          .then((validated) => {
            if (!active || sequence !== renderSequence.current) return
            setValidateResponse(validated)
            setValidatedConfigHash(validated.configHash)
            setValidatePending(false)
          })
          .catch((error) => {
            if (!active || sequence !== renderSequence.current) return
            setValidatePending(false)
            setValidateError(error instanceof Error ? error.message : "Failed to validate workflow YAML")
          })
      })
      .catch((error) => {
        if (!active || sequence !== renderSequence.current) return
        setRenderPending(false)
        setValidatePending(false)
        setRenderError(error instanceof Error ? error.message : "Failed to render workflow YAML")
      })

    return () => {
      active = false
    }
  }, [open, renderInput, renderReadiness.ready, selectedPattern, selectedWorkspace?.config, workspaceName])

  useEffect(() => {
    let active = true
    queueMicrotask(() => {
      if (!active) return
      setWarningSaveConfirmed(false)
      setSaveError("")
    })
    return () => {
      active = false
    }
  }, [currentConfigHash])

  useEffect(() => {
    if (!savedResponse) return
    if (!currentConfigHash || currentConfigHash !== savedResponse.readiness.configHash) {
      let active = true
      queueMicrotask(() => {
        if (active) setSavedResponse(null)
      })
      return () => {
        active = false
      }
    }
    return undefined
  }, [currentConfigHash, savedResponse])

  const updateDraft = useCallback((patch: Partial<WorkflowSetupDraft>) => {
    setDraft((current) => ({ ...current, ...patch }))
  }, [])

  const selectPattern = useCallback((patternId: string) => {
    setDraft((current) => ({ ...current, patternId }))
  }, [])

  const copyPreview = useCallback(async () => {
    if (!renderResponse?.config) return
    await navigator.clipboard.writeText(renderResponse.config)
    const copiedHash = renderResponse.configHash
    setCopiedConfigHash(copiedHash)
    window.setTimeout(() => {
      setCopiedConfigHash((current) => (current === copiedHash ? "" : current))
    }, 1500)
  }, [renderResponse])

  const focusStep = useCallback((stepId: StepId) => {
    setActiveStep(stepId)
    window.setTimeout(() => {
      stepButtonRefs.current[stepId]?.focus()
    }, 0)
  }, [])

  const focusFirstInvalidDraftField = useCallback((errors: DraftFieldErrors) => {
    const stepId = firstDraftErrorStep(errors)
    if (!stepId) return
    setActiveStep(stepId)
    window.setTimeout(() => {
      const invalid = document.querySelector<HTMLElement>('[data-workflow-setup-invalid="true"]')
      invalid?.focus()
    }, 0)
  }, [])

  const focusFirstFixableCritical = useCallback(() => {
    setActiveStep("review")
    window.setTimeout(() => {
      const fix = document.querySelector<HTMLElement>('[data-workflow-critical-fix="true"]')
      if (fix) {
        fix.focus()
        return
      }
      document.querySelector<HTMLElement>('[data-workflow-critical="true"]')?.focus()
    }, 0)
  }, [])

  const handleStepKeyDown = useCallback((event: KeyboardEvent<HTMLButtonElement>, stepId: StepId) => {
    const currentIndex = STEPS.findIndex((step) => step.id === stepId)
    if (currentIndex < 0) return

    let nextIndex = currentIndex
    switch (event.key) {
      case "ArrowDown":
      case "ArrowRight":
        nextIndex = (currentIndex + 1) % STEPS.length
        break
      case "ArrowUp":
      case "ArrowLeft":
        nextIndex = (currentIndex - 1 + STEPS.length) % STEPS.length
        break
      case "Home":
        nextIndex = 0
        break
      case "End":
        nextIndex = STEPS.length - 1
        break
      default:
        return
    }

    event.preventDefault()
    focusStep(STEPS[nextIndex].id)
  }, [focusStep])

  const handleSave = useCallback(async (allowWarnings: boolean) => {
    setAttemptedSave(true)
    setSaveError("")

    if (savePending) return

    const currentDraftErrors = getDraftFieldErrors(selectedPattern, draft, renderInput)
    if (Object.keys(currentDraftErrors).length > 0) {
      setSaveError("Complete the required fields before saving.")
      focusFirstInvalidDraftField(currentDraftErrors)
      return
    }

    if (saveBlockReason) {
      setSaveError(saveBlockReason)
      if (hasCriticals) focusFirstFixableCritical()
      return
    }

    if (hasWarnings && !allowWarnings) {
      setSaveError("Warnings require Save with warnings.")
      setActiveStep("review")
      return
    }

    if (hasWarnings && allowWarnings && !warningSaveConfirmed) {
      setSaveError("Confirm warnings before saving.")
      setActiveStep("review")
      return
    }

    if (!renderResponse?.config || !validatedConfigHash) {
      setSaveError("Rendered workflow YAML is not ready to save.")
      return
    }

    setSavePending(true)
    setSavedResponse(null)
    try {
      const saved = await saveWorkflowSetup({
        workspace: workspaceName,
        workflow: {
          name: renderResponse.workflowName || workflowName,
          config: renderResponse.config,
        },
        mode: "create",
        validatedConfigHash,
        allowWarnings,
      })
      if (!saved.saved) {
        throw new Error("Workflow save did not persist.")
      }
      setSavedResponse(saved)
      setWarningSaveConfirmed(false)
      setActiveStep("review")
      onWorkflowSaved?.(saved)
    } catch (error) {
      setSaveError(error instanceof Error ? error.message : "Failed to save workflow")
    } finally {
      setSavePending(false)
    }
  }, [
    draft,
    focusFirstFixableCritical,
    focusFirstInvalidDraftField,
    hasCriticals,
    hasWarnings,
    onWorkflowSaved,
    renderInput,
    renderResponse,
    saveBlockReason,
    savePending,
    selectedPattern,
    validatedConfigHash,
    warningSaveConfirmed,
    workflowName,
    workspaceName,
  ])

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="!w-full gap-0 p-0 sm:!max-w-5xl">
        <SheetHeader className="border-b border-border px-6 py-5 pr-12">
          <div className="flex items-start gap-3">
            <div className="flex size-9 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
              <GitBranch className="size-4" />
            </div>
            <div className="min-w-0">
              <SheetTitle>New Workflow</SheetTitle>
              <SheetDescription>
                Build a workflow draft for the current workspace.
              </SheetDescription>
            </div>
          </div>
        </SheetHeader>

        <div className="min-h-0 flex-1 overflow-hidden">
          <div className="grid h-full min-h-0 md:grid-cols-[220px_minmax(0,1fr)]">
            <aside className="border-b border-border bg-muted/20 px-4 py-4 md:border-b-0 md:border-r">
              <div className="mb-4 rounded-md border border-border bg-background p-3">
                <p className="text-xs font-medium text-muted-foreground">Workspace</p>
                <div className="mt-1 flex items-center gap-2">
                  <p className="min-w-0 flex-1 truncate text-sm font-medium">{workspaceName || "No workspace"}</p>
                  <Badge variant="secondary">Selected</Badge>
                </div>
                <p className="mt-1 text-xs text-muted-foreground">
                  {selectedWorkspace
                    ? `${selectedWorkspace.workflows?.length ?? 0} workflow${(selectedWorkspace.workflows?.length ?? 0) === 1 ? "" : "s"} configured`
                    : "Choose an existing workspace before saving."}
                </p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="mt-3 w-full"
                  onClick={onChangeWorkspace}
                >
                  Change workspace
                </Button>
              </div>

              <nav aria-label="Workflow setup steps" aria-orientation="vertical" role="tablist" className="space-y-1">
                {STEPS.map((step, index) => {
                  const selected = activeStep === step.id
                  const Icon = step.icon
                  return (
                    <button
                      key={step.id}
                      id={`workflow-setup-tab-${step.id}`}
                      ref={(node) => {
                        stepButtonRefs.current[step.id] = node
                      }}
                      type="button"
                      role="tab"
                      aria-selected={selected}
                      aria-controls={`workflow-setup-step-${step.id}`}
                      tabIndex={selected ? 0 : -1}
                      onClick={() => setActiveStep(step.id)}
                      onKeyDown={(event) => handleStepKeyDown(event, step.id)}
                      className={cn(
                        "flex w-full items-center gap-3 rounded-md px-3 py-2 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                        selected ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:bg-background/70 hover:text-foreground"
                      )}
                    >
                      <span
                        className={cn(
                          "flex size-6 shrink-0 items-center justify-center rounded-md border text-xs",
                          selected ? "border-primary/30 bg-primary/10 text-primary" : "border-border bg-background"
                        )}
                      >
                        {index + 1}
                      </span>
                      <Icon className="size-4 shrink-0" />
                      <span className="truncate">{step.label}</span>
                    </button>
                  )
                })}
              </nav>
            </aside>

            <main className="min-h-0 overflow-y-auto px-6 py-5">
              <div className="mx-auto max-w-3xl">
                <StepPanel active={activeStep === "pattern"} id="pattern">
                  <PatternStep
                    patterns={patterns}
                    patternsError={patternsError}
                    patternsLoading={patternsLoading}
                    selectedPatternId={draft.patternId}
                    workflowName={draft.workflowName}
                    workflowNameError={draftFieldErrors.workflowName}
                    onSelectPattern={selectPattern}
                    onWorkflowNameChange={(workflowName) => updateDraft({ workflowName })}
                  />
                </StepPanel>

                <StepPanel active={activeStep === "access"} id="access">
                  <AccessStep
                    contextError={contextError}
                    contextLoading={contextLoading}
                    requirements={accessRequirements}
                    selectedPattern={selectedPattern}
                  />
                </StepPanel>

                <StepPanel active={activeStep === "trigger"} id="trigger">
                  <TriggerStep
                    draft={draft}
                    selectedPattern={selectedPattern}
                    setupContext={setupContext}
                    fieldErrors={draftFieldErrors}
                    workflowName={workflowName}
                    onChange={updateDraft}
                  />
                </StepPanel>

                <StepPanel active={activeStep === "lifecycle"} id="lifecycle">
                  <LifecycleStep draft={draft} selectedPattern={selectedPattern} onChange={updateDraft} />
                </StepPanel>

                <StepPanel active={activeStep === "review"} id="review">
                  <ReviewStep
                    copiedPreview={copiedPreview}
                    currentConfigHash={currentConfigHash}
                    diagnostics={diagnosticGroups}
                    renderError={renderError}
                    renderPending={renderPending}
                    renderReadiness={renderReadiness}
                    renderResponse={renderResponse}
                    validateError={validateError}
                    validatePending={validatePending}
                    validateResponse={validateResponse}
                    validationMatchesCurrent={validationMatchesCurrent}
                    savedResponse={savedResponse}
                    onCopyPreview={copyPreview}
                  />
                </StepPanel>
              </div>
            </main>
          </div>
        </div>

        <SheetFooter className="sticky bottom-0 z-10 mt-0 flex-col items-stretch gap-3 border-t border-border bg-background px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-6">
          <div className="min-w-0">
            <p className="text-xs font-medium text-muted-foreground">Workflow setup</p>
            <p className={cn("mt-0.5 text-xs text-muted-foreground", saveError && "text-red-500")}>{footerStatus}</p>
            <p className="sr-only" aria-live="polite" aria-atomic="true">{liveStatus}</p>
          </div>
          <div className="flex w-full flex-col gap-2 sm:w-auto sm:flex-row sm:flex-wrap sm:justify-end">
            <Button type="button" variant="outline" className="w-full sm:w-auto" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              type="button"
              className="w-full sm:w-auto"
              disabled={!canSaveWorkflow}
              onClick={() => void handleSave(false)}
            >
              {savePending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
              Save workflow
            </Button>
            {hasWarnings && (
              <>
                <label className="flex min-h-8 items-center gap-2 rounded-md border border-amber-500/30 px-3 py-2 text-xs text-muted-foreground sm:min-h-9">
                  <input
                    type="checkbox"
                    checked={warningSaveConfirmed}
                    onChange={(event) => setWarningSaveConfirmed(event.target.checked)}
                    disabled={savePending || !saveReady}
                    className="size-4"
                  />
                  <span>Reviewed warnings</span>
                </label>
                <Button
                  type="button"
                  variant="secondary"
                  className="w-full sm:w-auto"
                  disabled={!canSaveWithWarnings}
                  onClick={() => void handleSave(true)}
                >
                  Save with warnings
                </Button>
              </>
            )}
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}

function StepPanel({ active, children, id }: { active: boolean; children: ReactNode; id: StepId }) {
  if (!active) return null
  return (
    <section
      id={`workflow-setup-step-${id}`}
      role="tabpanel"
      tabIndex={0}
      aria-labelledby={`workflow-setup-tab-${id}`}
      className="outline-none"
    >
      {children}
    </section>
  )
}

function PatternStep({
  patterns,
  patternsError,
  patternsLoading,
  selectedPatternId,
  workflowName,
  workflowNameError,
  onSelectPattern,
  onWorkflowNameChange,
}: {
  patterns: PatternMetadata[]
  patternsError: string
  patternsLoading: boolean
  selectedPatternId: string
  workflowName: string
  workflowNameError?: string
  onSelectPattern: (patternId: string) => void
  onWorkflowNameChange: (workflowName: string) => void
}) {
  const selectedPattern = patterns.find((pattern) => pattern.id === selectedPatternId)
  const workflowNameDescriptionId = "workflow-setup-name-description"
  const workflowNameErrorId = "workflow-setup-name-error"
  return (
    <div className="space-y-6">
      <StepHeading
        icon={LayoutTemplate}
        title="Pattern"
        description="Choose the workflow shape and name the generated YAML."
      />

      <section className="space-y-3">
        <FieldLabel
          htmlFor="workflow-setup-name"
          label="Workflow name"
          description="Use a stable slug for this workflow."
          descriptionId={workflowNameDescriptionId}
        />
        <Input
          id="workflow-setup-name"
          value={workflowName}
          onChange={(event) => onWorkflowNameChange(event.target.value)}
          placeholder={selectedPattern?.id ?? "issue-triage"}
          autoComplete="off"
          aria-describedby={workflowNameError ? `${workflowNameDescriptionId} ${workflowNameErrorId}` : workflowNameDescriptionId}
          aria-invalid={Boolean(workflowNameError) || undefined}
          data-workflow-setup-invalid={workflowNameError ? "true" : undefined}
        />
        {workflowNameError && (
          <p id={workflowNameErrorId} className="text-xs text-red-500">
            {workflowNameError}
          </p>
        )}
      </section>

      <section className="space-y-3">
        <div>
          <h3 className="text-sm font-medium">Pattern</h3>
          <p className="text-xs text-muted-foreground">Defaults and required fields come from the backend metadata.</p>
        </div>
        {patternsError && (
          <Notice tone="critical" icon={AlertTriangle}>
            {patternsError}
          </Notice>
        )}
        {patternsLoading && patterns.length === 0 ? (
          <div className="flex items-center gap-2 rounded-md border border-border px-4 py-5 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            Loading workflow patterns...
          </div>
        ) : patterns.length === 0 && !patternsError ? (
          <p className="rounded-md border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">
            No setup patterns available.
          </p>
        ) : (
          <div className="grid gap-2 sm:grid-cols-2">
            {patterns.map((pattern) => {
              const selected = selectedPatternId === pattern.id
              return (
                <button
                  key={pattern.id}
                  type="button"
                  aria-pressed={selected}
                  onClick={() => onSelectPattern(pattern.id)}
                  className={cn(
                    "rounded-md border border-border p-4 text-left transition-colors hover:bg-muted/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    selected && "border-primary bg-primary/5"
                  )}
                >
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <p className="text-sm font-medium">{pattern.label}</p>
                      <p className="mt-1 text-xs text-muted-foreground">{pattern.description}</p>
                      <p className="mt-2 text-xs text-muted-foreground">
                        {pattern.requiredFields.length} required, {pattern.advancedFields.length} advanced
                      </p>
                    </div>
                    {selected && <CheckCircle2 className="size-4 shrink-0 text-primary" />}
                  </div>
                </button>
              )
            })}
          </div>
        )}
      </section>

      {selectedPattern && (
        <section className="space-y-2">
          <h3 className="text-sm font-medium">Backend fields</h3>
          <div className="grid gap-2 sm:grid-cols-2">
            <FieldList title="Required" fields={selectedPattern.requiredFields} />
            <FieldList title="Advanced" fields={selectedPattern.advancedFields} />
          </div>
        </section>
      )}
    </div>
  )
}

function AccessStep({
  contextError,
  contextLoading,
  requirements,
  selectedPattern,
}: {
  contextError: string
  contextLoading: boolean
  requirements: AccessRequirement[]
  selectedPattern?: PatternMetadata
}) {
  return (
    <div className="space-y-6">
      <StepHeading
        icon={ShieldCheck}
        title="Access"
        description="Review only the resources used by the selected pattern."
      />

      {!selectedPattern ? (
        <Notice tone="info" icon={Info}>
          Select a pattern before reviewing access.
        </Notice>
      ) : contextLoading ? (
        <div className="flex items-center gap-2 rounded-md border border-border px-4 py-5 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" />
          Loading workspace context...
        </div>
      ) : contextError ? (
        <Notice tone="critical" icon={AlertTriangle}>
          {contextError}
        </Notice>
      ) : (
        <div className="space-y-3">
          {requirements.map((requirement) => (
            <AccessRequirementRow key={requirement.id} requirement={requirement} />
          ))}
        </div>
      )}
    </div>
  )
}

function TriggerStep({
  draft,
  fieldErrors,
  selectedPattern,
  setupContext,
  workflowName,
  onChange,
}: {
  draft: WorkflowSetupDraft
  fieldErrors: DraftFieldErrors
  selectedPattern?: PatternMetadata
  setupContext: SetupContext | null
  workflowName: string
  onChange: (patch: Partial<WorkflowSetupDraft>) => void
}) {
  if (!selectedPattern) {
    return (
      <div className="space-y-6">
        <StepHeading icon={Play} title="Trigger" description="Configure the event that starts the workflow." />
        <Notice tone="info" icon={Info}>
          Select a pattern before configuring its trigger.
        </Notice>
      </div>
    )
  }

  const defaults = selectedPattern.defaults
  const repositoryOptions = arrayValue(setupContext?.workspace.repositories).map((repository) => repository.repo)
  const linearWorkspaces = issueTrackerWorkspaces(setupContext, "linear")
  const shortcutWorkspaces = issueTrackerWorkspaces(setupContext, "shortcut")
  const concurrencyGroups = arrayValue(setupContext?.concurrencyGroups).map((group) => group.name)
  const effectiveManualTrigger = draft.enableManualTrigger ?? booleanDefault(defaults, "enableManualTrigger", false)
  const effectiveManualRequired = draft.manualInputRequired ?? manualInputDefault(selectedPattern, "required", false)

  return (
    <div className="space-y-6">
      <StepHeading icon={Play} title="Trigger" description="Fill the minimum fields required for this pattern." />

      {selectedPattern.id === "github-issue" && (
        <div className="space-y-4">
          <Datalist id="workflow-setup-repositories" values={repositoryOptions} />
          <TextField
            id="workflow-setup-repository"
            label="Repository"
            description="GitHub repository in owner/repo format."
            value={draft.repository}
            onChange={(repository) => onChange({ repository })}
            placeholder="owner/repo"
            list="workflow-setup-repositories"
            error={fieldErrors.repository}
          />
          <TextField
            id="workflow-setup-trigger-label"
            label="Label / trigger label"
            description="The label that starts work and is removed when the workflow enters the working stage."
            value={draft.triggerLabel}
            onChange={(triggerLabel) => onChange({ triggerLabel })}
            placeholder={stringDefault(defaults, "triggerLabel")}
          />
          <SwitchField
            id="workflow-setup-manual-enabled"
            label="Manual enabled"
            description="Allow a manual run with an issue number input."
            checked={effectiveManualTrigger}
            onCheckedChange={(enableManualTrigger) => onChange({ enableManualTrigger })}
          />
          <ConcurrencyField
            value={draft.concurrencyGroup}
            defaultValue={stringDefault(defaults, "concurrencyGroup")}
            options={concurrencyGroups}
            onChange={(concurrencyGroup) => onChange({ concurrencyGroup })}
          />
        </div>
      )}

      {selectedPattern.id === "linear-status" && (
        <div className="space-y-4">
          <Datalist id="workflow-setup-linear-workspaces" values={linearWorkspaces} />
          <TextField
            id="workflow-setup-linear-workspace"
            label="Issue tracker workspace"
            description="Linear workspace configured in settings."
            value={draft.trackerWorkspace}
            onChange={(trackerWorkspace) => onChange({ trackerWorkspace })}
            placeholder={linearWorkspaces[0] ?? ""}
            list="workflow-setup-linear-workspaces"
            error={fieldErrors.trackerWorkspace}
          />
          <TextField
            id="workflow-setup-linear-trigger-status"
            label="Trigger status"
            description="Linear status that starts the workflow."
            value={draft.triggerStatus}
            onChange={(triggerStatus) => onChange({ triggerStatus })}
            placeholder={stringDefault(defaults, "triggerStatus")}
          />
          <ConcurrencyField
            value={draft.concurrencyGroup}
            defaultValue={stringDefault(defaults, "concurrencyGroup")}
            options={concurrencyGroups}
            onChange={(concurrencyGroup) => onChange({ concurrencyGroup })}
          />
        </div>
      )}

      {selectedPattern.id === "shortcut-status" && (
        <div className="space-y-4">
          <Datalist id="workflow-setup-shortcut-workspaces" values={shortcutWorkspaces} />
          <TextField
            id="workflow-setup-shortcut-workspace"
            label="Issue tracker workspace"
            description="Shortcut workspace configured in settings."
            value={draft.trackerWorkspace}
            onChange={(trackerWorkspace) => onChange({ trackerWorkspace })}
            placeholder={shortcutWorkspaces[0] ?? ""}
            list="workflow-setup-shortcut-workspaces"
            error={fieldErrors.trackerWorkspace}
          />
          <TextField
            id="workflow-setup-shortcut-trigger-status"
            label="Trigger state"
            description="Shortcut workflow state that starts the workflow."
            value={draft.triggerStatus}
            onChange={(triggerStatus) => onChange({ triggerStatus })}
            placeholder={stringDefault(defaults, "triggerStatus")}
          />
          <ConcurrencyField
            value={draft.concurrencyGroup}
            defaultValue={stringDefault(defaults, "concurrencyGroup")}
            options={concurrencyGroups}
            onChange={(concurrencyGroup) => onChange({ concurrencyGroup })}
          />
        </div>
      )}

      {selectedPattern.id === "manual-task" && (
        <div className="space-y-4">
          <Notice tone="info" icon={Info}>
            {workflowName ? "Manual workflows start from a user action." : "Set a workflow name in Pattern before rendering."}
          </Notice>
          <TextField
            id="workflow-setup-manual-input-name"
            label="Manual input"
            description="Basic input shown when the manual workflow starts."
            value={draft.manualInputName}
            onChange={(manualInputName) => onChange({ manualInputName })}
            placeholder={manualInputDefault(selectedPattern, "name", "")}
            error={fieldErrors.manualInputName}
          />
          <TextField
            id="workflow-setup-manual-input-description"
            label="Input description"
            description="Short prompt for the person starting the workflow."
            value={draft.manualInputDescription}
            onChange={(manualInputDescription) => onChange({ manualInputDescription })}
            placeholder={manualInputDefault(selectedPattern, "description", "")}
          />
          <div className="grid gap-4 sm:grid-cols-2">
            <TextField
              id="workflow-setup-manual-input-type"
              label="Input type"
              description="Use a backend-supported input type."
              value={draft.manualInputType}
              onChange={(manualInputType) => onChange({ manualInputType })}
              placeholder={manualInputDefault(selectedPattern, "type", "")}
            />
            <TextField
              id="workflow-setup-manual-input-default"
              label="Default value"
              description="Optional default for this manual input."
              value={draft.manualInputDefault}
              onChange={(manualInputDefaultValue) => onChange({ manualInputDefault: manualInputDefaultValue })}
              placeholder={manualInputDefault(selectedPattern, "default", "")}
            />
          </div>
          <SwitchField
            id="workflow-setup-manual-input-required"
            label="Required input"
            description="Require a value before starting the workflow."
            checked={effectiveManualRequired}
            onCheckedChange={(manualInputRequired) => onChange({ manualInputRequired })}
          />
          <ConcurrencyField
            value={draft.concurrencyGroup}
            defaultValue={stringDefault(defaults, "concurrencyGroup")}
            options={concurrencyGroups}
            onChange={(concurrencyGroup) => onChange({ concurrencyGroup })}
          />
        </div>
      )}
    </div>
  )
}

function LifecycleStep({
  draft,
  selectedPattern,
  onChange,
}: {
  draft: WorkflowSetupDraft
  selectedPattern?: PatternMetadata
  onChange: (patch: Partial<WorkflowSetupDraft>) => void
}) {
  if (!selectedPattern) {
    return (
      <div className="space-y-6">
        <StepHeading icon={Workflow} title="Lifecycle" description="Configure optional state changes." />
        <Notice tone="info" icon={Info}>
          Select a pattern before configuring lifecycle fields.
        </Notice>
      </div>
    )
  }

  const defaults = selectedPattern.defaults
  const includePreCommit = draft.includePreCommit ?? booleanDefault(defaults, "includePreCommit", false)

  return (
    <div className="space-y-6">
      <StepHeading
        icon={Workflow}
        title="Lifecycle"
        description="Optional labels, issue states, and stage behavior rendered by the backend."
      />

      {selectedPattern.id === "github-issue" && (
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField
            id="workflow-setup-working-label"
            label="Working label"
            description="Added when work starts."
            value={draft.workingStatus}
            onChange={(workingStatus) => onChange({ workingStatus })}
            placeholder={stringDefault(defaults, "workingLabel")}
          />
          <TextField
            id="workflow-setup-review-label"
            label="Review label"
            description="Added when a pull request opens."
            value={draft.prOpenedStatus}
            onChange={(prOpenedStatus) => onChange({ prOpenedStatus })}
            placeholder={stringDefault(defaults, "reviewLabel")}
          />
          <TextField
            id="workflow-setup-done-label"
            label="Done label"
            description="Added when a pull request merges."
            value={draft.mergedStatus}
            onChange={(mergedStatus) => onChange({ mergedStatus })}
            placeholder={stringDefault(defaults, "doneLabel")}
          />
          <TextField
            id="workflow-setup-closed-label"
            label="Canceled label"
            description="Added when a pull request closes without merging."
            value={draft.closedNoMergeStatus}
            onChange={(closedNoMergeStatus) => onChange({ closedNoMergeStatus })}
            placeholder={stringDefault(defaults, "closedLabel")}
          />
        </div>
      )}

      {(selectedPattern.id === "linear-status" || selectedPattern.id === "shortcut-status") && (
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField
            id="workflow-setup-working-status"
            label={selectedPattern.id === "linear-status" ? "Working status" : "Working state"}
            description="Optional state when work starts."
            value={draft.workingStatus}
            onChange={(workingStatus) => onChange({ workingStatus })}
            placeholder={stringDefault(defaults, "workingStatus")}
          />
          <TextField
            id="workflow-setup-review-status"
            label={selectedPattern.id === "linear-status" ? "PR opened status" : "PR opened state"}
            description="Optional state when a pull request opens."
            value={draft.prOpenedStatus}
            onChange={(prOpenedStatus) => onChange({ prOpenedStatus })}
            placeholder={stringDefault(defaults, "prOpenedStatus")}
          />
          <TextField
            id="workflow-setup-done-status"
            label={selectedPattern.id === "linear-status" ? "Done status" : "Done state"}
            description="Optional state when a pull request merges."
            value={draft.mergedStatus}
            onChange={(mergedStatus) => onChange({ mergedStatus })}
            placeholder={stringDefault(defaults, "mergedStatus")}
          />
          <TextField
            id="workflow-setup-canceled-status"
            label={selectedPattern.id === "linear-status" ? "Canceled status" : "Canceled state"}
            description="Optional state when a pull request closes without merging."
            value={draft.closedNoMergeStatus}
            onChange={(closedNoMergeStatus) => onChange({ closedNoMergeStatus })}
            placeholder={stringDefault(defaults, "closedNoMergeStatus")}
          />
        </div>
      )}

      {selectedPattern.id === "manual-task" && (
        <TextField
          id="workflow-setup-done-signal"
          label="Done signal"
          description="Message token that completes the manual workflow."
          value={draft.doneSignal}
          onChange={(doneSignal) => onChange({ doneSignal })}
          placeholder={stringDefault(defaults, "doneSignal")}
        />
      )}

      <section className="space-y-4 rounded-md border border-border p-4">
        <SwitchField
          id="workflow-setup-precommit-enabled"
          label="Pre-commit stage"
          description="Add a command stage before handoff or completion."
          checked={includePreCommit}
          onCheckedChange={(includePreCommitValue) => onChange({ includePreCommit: includePreCommitValue })}
        />
        {includePreCommit && (
          <TextField
            id="workflow-setup-precommit-command"
            label="Pre-commit command"
            description="Command rendered into the pre-commit stage."
            value={draft.preCommitCommand}
            onChange={(preCommitCommand) => onChange({ preCommitCommand })}
            placeholder={stringDefault(defaults, "preCommitCommand")}
          />
        )}
      </section>
    </div>
  )
}

function ReviewStep({
  copiedPreview,
  currentConfigHash,
  diagnostics,
  renderError,
  renderPending,
  renderReadiness,
  renderResponse,
  validateError,
  validatePending,
  validateResponse,
  validationMatchesCurrent,
  savedResponse,
  onCopyPreview,
}: {
  copiedPreview: boolean
  currentConfigHash: string
  diagnostics: Record<Diagnostic["severity"], Diagnostic[]>
  renderError: string
  renderPending: boolean
  renderReadiness: { ready: boolean; reason: string }
  renderResponse: RenderResponse | null
  validateError: string
  validatePending: boolean
  validateResponse: ValidateResponse | null
  validationMatchesCurrent: boolean
  savedResponse: SaveResponse | null
  onCopyPreview: () => void
}) {
  const validationStatus = getValidationStatus({
    currentConfigHash,
    renderError,
    renderPending,
    renderReadiness,
    validateError,
    validatePending,
    validateResponse,
    validationMatchesCurrent,
  })

  return (
    <div className="space-y-6">
      <StepHeading
        icon={ClipboardList}
        title="Review"
        description="Preview read-only YAML and validation diagnostics before saving."
      />

      {savedResponse && <FinishPanel response={savedResponse} />}

      <section className="space-y-3">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <h3 className="text-sm font-medium">Validation</h3>
            <p className="text-xs text-muted-foreground">{validationStatus}</p>
          </div>
          <ValidationSummary response={validateResponse} pending={validatePending || renderPending} />
        </div>

        {renderError && (
          <Notice tone="critical" icon={AlertTriangle}>
            {renderError}
          </Notice>
        )}
        {validateError && (
          <Notice tone="critical" icon={AlertTriangle}>
            {validateError}
          </Notice>
        )}
        {!renderReadiness.ready && (
          <Notice tone="info" icon={Info}>
            {renderReadiness.reason}
          </Notice>
        )}
      </section>

      <section className="space-y-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <h3 className="text-sm font-medium">YAML preview</h3>
            <p className="text-xs text-muted-foreground">
              Read-only for this release. Copy the rendered config if you need to inspect it elsewhere.
            </p>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onCopyPreview}
            disabled={!renderResponse?.config}
          >
            {copiedPreview ? <Check className="size-4" /> : <Copy className="size-4" />}
            {copiedPreview ? "Copied" : "Copy"}
          </Button>
        </div>
        <Textarea
          readOnly
          value={
            renderResponse?.config ??
            (renderPending ? "Rendering workflow YAML..." : "Complete the required fields to render a preview.")
          }
          className="min-h-[340px] resize-y font-mono text-xs"
          aria-label="Rendered workflow YAML preview"
        />
      </section>

      <DiagnosticList diagnostics={diagnostics} />
    </div>
  )
}

function FinishPanel({ response }: { response: SaveResponse }) {
  const readyToRun = isSavedWorkflowReadyToRun(response)
  const summary = response.readiness.summary
  const manualTriggerAvailable = Boolean(response.workflow.enableManualTrigger)

  return (
    <section className="space-y-4 rounded-md border border-border p-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h3 className="text-sm font-medium">Finish</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            Saved {response.workflow.name} in {response.workspace}.
          </p>
        </div>
        <Badge variant={readyToRun ? "secondary" : "outline"}>
          {readyToRun ? "Ready to run" : "Saved as draft"}
        </Badge>
      </div>

      {readyToRun ? (
        <Notice tone="success" icon={CheckCircle2}>
          Ready to run. No critical checks or warning prerequisites were reported.
        </Notice>
      ) : (
        <Notice tone="warning" icon={AlertTriangle}>
          Saved as draft. Resolve remaining prerequisites before treating this workflow as runnable.
        </Notice>
      )}

      {(summary.critical > 0 || summary.warning > 0 || !response.workflow.enabled) && (
        <div className="grid gap-2 text-xs text-muted-foreground sm:grid-cols-3">
          <div className="rounded-md border border-border px-3 py-2">
            <p className="font-medium text-foreground">{summary.critical}</p>
            <p>Critical</p>
          </div>
          <div className="rounded-md border border-border px-3 py-2">
            <p className="font-medium text-foreground">{summary.warning}</p>
            <p>Warnings</p>
          </div>
          <div className="rounded-md border border-border px-3 py-2">
            <p className="font-medium text-foreground">{response.workflow.enabled ? "Enabled" : "Paused"}</p>
            <p>Auto handling</p>
          </div>
        </div>
      )}

      {readyToRun && manualTriggerAvailable && (
        <ManualTriggerForm workflow={response.workflow} />
      )}
      {!readyToRun && manualTriggerAvailable && (
        <Notice tone="info" icon={Info}>
          Manual trigger will be available when this workflow is Ready to run.
        </Notice>
      )}
    </section>
  )
}

function ManualTriggerForm({ workflow }: { workflow: SavedWorkflow }) {
  const inputs = useMemo(() => manualTriggerInputsForWorkflow(workflow), [workflow])
  const [values, setValues] = useState<Record<string, string>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [triggerPending, setTriggerPending] = useState(false)
  const [triggerError, setTriggerError] = useState("")
  const [triggerSuccess, setTriggerSuccess] = useState("")

  useEffect(() => {
    let active = true
    const defaults: Record<string, string> = {}
    for (const input of inputs) {
      if (input.default !== undefined) {
        defaults[input.name] = input.default
      } else if (input.type === "bool") {
        defaults[input.name] = "false"
      } else if (input.type === "enum" && input.options?.length) {
        defaults[input.name] = input.options[0]
      }
    }
    queueMicrotask(() => {
      if (!active) return
      setValues(defaults)
      setErrors({})
      setTriggerError("")
      setTriggerSuccess("")
    })
    return () => {
      active = false
    }
  }, [inputs])

  const updateValue = useCallback((name: string, value: string) => {
    setValues((current) => ({ ...current, [name]: value }))
    setErrors((current) => {
      if (!current[name]) return current
      const next = { ...current }
      delete next[name]
      return next
    })
    setTriggerError("")
    setTriggerSuccess("")
  }, [])

  const submitTrigger = useCallback(async () => {
    setTriggerError("")
    setTriggerSuccess("")

    const validation = buildManualTriggerInputs(workflow, inputs, values)
    if (!validation.ok) {
      setErrors(validation.errors)
      return
    }

    setTriggerPending(true)
    try {
      const result = await triggerWorkflow(workflow, validation.inputs)
      setTriggerSuccess(result.claw_id ? `Started claw ${result.claw_id}.` : `Trigger returned ${result.status || "success"}.`)
    } catch (error) {
      setTriggerError(error instanceof Error ? error.message : "Failed to trigger workflow")
    } finally {
      setTriggerPending(false)
    }
  }, [inputs, values, workflow])

  return (
    <section className="space-y-3 border-t border-border pt-4">
      <div>
        <h4 className="text-sm font-medium">Manual trigger</h4>
        <p className="mt-1 text-xs text-muted-foreground">
          Run this saved workflow after the trigger endpoint accepts the inputs.
        </p>
      </div>

      {inputs.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2">
          {inputs.map((input) => (
            <ManualTriggerInput
              key={input.name}
              input={input}
              value={values[input.name] ?? ""}
              error={errors[input.name]}
              onChange={(value) => updateValue(input.name, value)}
            />
          ))}
        </div>
      )}

      {triggerError && (
        <Notice tone="critical" icon={AlertTriangle}>
          {triggerError}
        </Notice>
      )}
      {triggerSuccess && (
        <Notice tone="success" icon={CheckCircle2}>
          {triggerSuccess}
        </Notice>
      )}
      <p className="sr-only" aria-live="polite" aria-atomic="true">
        {triggerPending ? "Triggering workflow." : triggerError || triggerSuccess}
      </p>

      <Button type="button" onClick={() => void submitTrigger()} disabled={triggerPending}>
        {triggerPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
        Run
      </Button>
    </section>
  )
}

function ManualTriggerInput({
  error,
  input,
  onChange,
  value,
}: {
  error?: string
  input: WorkflowInput
  onChange: (value: string) => void
  value: string
}) {
  const id = `workflow-finish-input-${input.name}`
  const descriptionId = `${id}-description`
  const errorId = `${id}-error`
  const required = input.required || input.name === "issue_number"

  return (
    <div className="space-y-2">
      <div className="space-y-1">
        <Label htmlFor={id}>
          {input.name}
          {required && <span className="ml-1 text-red-500">*</span>}
        </Label>
        <p id={descriptionId} className="text-xs text-muted-foreground">
          {input.description || `${input.type} input`}
        </p>
      </div>
      {input.type === "enum" && input.options?.length ? (
        <select
          id={id}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          aria-describedby={error ? `${descriptionId} ${errorId}` : descriptionId}
          aria-invalid={Boolean(error) || undefined}
          className="h-9 w-full rounded-md border border-border bg-background px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {input.options.map((option) => (
            <option key={option} value={option}>{option}</option>
          ))}
        </select>
      ) : input.type === "bool" ? (
        <select
          id={id}
          value={value || "false"}
          onChange={(event) => onChange(event.target.value)}
          aria-describedby={error ? `${descriptionId} ${errorId}` : descriptionId}
          aria-invalid={Boolean(error) || undefined}
          className="h-9 w-full rounded-md border border-border bg-background px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      ) : (
        <Input
          id={id}
          type={input.type === "number" || input.name === "issue_number" ? "number" : "text"}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          placeholder={input.default}
          min={input.type === "number" || input.name === "issue_number" ? input.min ?? 1 : undefined}
          step={input.name === "issue_number" ? 1 : undefined}
          aria-describedby={error ? `${descriptionId} ${errorId}` : descriptionId}
          aria-invalid={Boolean(error) || undefined}
        />
      )}
      {error && (
        <p id={errorId} className="text-xs text-red-500">
          {error}
        </p>
      )}
    </div>
  )
}

function StepHeading({
  description,
  icon: Icon,
  title,
}: {
  description: string
  icon: ElementType
  title: string
}) {
  return (
    <div className="flex items-start gap-3">
      <div className="flex size-9 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
        <Icon className="size-4" />
      </div>
      <div>
        <h2 className="text-base font-semibold">{title}</h2>
        <p className="mt-1 text-sm text-muted-foreground">{description}</p>
      </div>
    </div>
  )
}

function FieldList({ fields, title }: { fields: Array<{ path: string; label: string; description: string }>; title: string }) {
  return (
    <div className="rounded-md border border-border p-3">
      <p className="text-xs font-medium text-muted-foreground">{title}</p>
      <div className="mt-2 space-y-2">
        {fields.map((field) => (
          <div key={field.path}>
            <p className="text-sm font-medium">{field.label}</p>
            <p className="text-xs text-muted-foreground">{field.description}</p>
          </div>
        ))}
      </div>
    </div>
  )
}

function AccessRequirementRow({ requirement }: { requirement: AccessRequirement }) {
  return (
    <div className="flex flex-col gap-3 rounded-md border border-border p-4 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <StatusBadge status={requirement.status} />
          <h3 className="text-sm font-medium">{requirement.label}</h3>
        </div>
        <p className="mt-1 text-sm text-muted-foreground">{requirement.description}</p>
        <p className="mt-2 text-xs text-muted-foreground">{requirement.detail}</p>
      </div>
      {requirement.status === "missing" && requirement.settingsHref && (
        <Button type="button" variant="outline" size="sm" asChild>
          <Link href={requirement.settingsHref}>
            <Settings className="size-4" />
            {requirement.settingsLabel ?? "Open settings"}
          </Link>
        </Button>
      )}
    </div>
  )
}

function DiagnosticList({ diagnostics }: { diagnostics: Record<Diagnostic["severity"], Diagnostic[]> }) {
  const total = diagnostics.critical.length + diagnostics.warning.length + diagnostics.info.length
  if (total === 0) {
    return (
      <Notice tone="success" icon={CheckCircle2}>
        No diagnostics reported for the current rendered config.
      </Notice>
    )
  }

  return (
    <section className="space-y-4">
      <div>
        <h3 className="text-sm font-medium">Diagnostics</h3>
        <p className="text-xs text-muted-foreground">Critical diagnostics are grouped first, followed by warnings and info.</p>
      </div>
      <DiagnosticGroup title="Critical" diagnostics={diagnostics.critical} tone="critical" />
      <DiagnosticGroup title="Warnings" diagnostics={diagnostics.warning} tone="warning" />
      <DiagnosticGroup title="Info" diagnostics={diagnostics.info} tone="info" />
    </section>
  )
}

function DiagnosticGroup({
  diagnostics,
  title,
  tone,
}: {
  diagnostics: Diagnostic[]
  title: string
  tone: "critical" | "warning" | "info"
}) {
  if (diagnostics.length === 0) return null
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <SeverityIcon severity={tone === "critical" ? "critical" : tone === "warning" ? "warning" : "info"} />
        <p className="text-sm font-medium">{title}</p>
        <Badge variant="outline">{diagnostics.length}</Badge>
      </div>
      <div className="space-y-2">
        {diagnostics.map((diagnostic) => {
          const cta = settingsLinkForDiagnostic(diagnostic)
          return (
            <div
              key={`${diagnostic.id}:${diagnostic.fieldPath}`}
              tabIndex={tone === "critical" ? -1 : undefined}
              data-workflow-critical={tone === "critical" ? "true" : undefined}
              className="rounded-md border border-border p-3 outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                <div className="min-w-0">
                  <p className="text-sm font-medium">{diagnostic.title}</p>
                  <p className="mt-1 text-xs text-muted-foreground">{diagnostic.detail}</p>
                  <p className="mt-2 font-mono text-[11px] text-muted-foreground">{diagnostic.fieldPath || diagnostic.fixTarget}</p>
                </div>
                {cta && (
                  <Button type="button" variant="outline" size="sm" asChild>
                    <Link href={cta.href} data-workflow-critical-fix={tone === "critical" ? "true" : undefined}>
                      <Settings className="size-4" />
                      {cta.label}
                    </Link>
                  </Button>
                )}
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}

function ValidationSummary({ pending, response }: { pending: boolean; response: ValidateResponse | null }) {
  if (pending) {
    return (
      <Badge variant="secondary" className="gap-1">
        <Loader2 className="size-3 animate-spin" />
        Checking
      </Badge>
    )
  }
  if (!response) return <Badge variant="outline">Not validated</Badge>
  if (response.summary.critical > 0) {
    return <Badge variant="destructive">{response.summary.critical} critical</Badge>
  }
  if (response.summary.warning > 0) {
    return <Badge variant="outline">{response.summary.warning} warning</Badge>
  }
  return <Badge variant="secondary">Validated</Badge>
}

function FieldLabel({
  description,
  descriptionId,
  htmlFor,
  label,
}: {
  description: string
  descriptionId?: string
  htmlFor: string
  label: string
}) {
  return (
    <div className="space-y-1">
      <Label htmlFor={htmlFor}>{label}</Label>
      <p id={descriptionId} className="text-xs text-muted-foreground">{description}</p>
    </div>
  )
}

function TextField({
  description,
  error,
  id,
  label,
  list,
  onChange,
  placeholder,
  value,
}: {
  description: string
  error?: string
  id: string
  label: string
  list?: string
  onChange: (value: string) => void
  placeholder?: string
  value: string
}) {
  const descriptionId = `${id}-description`
  const errorId = `${id}-error`
  return (
    <div className="space-y-2">
      <FieldLabel htmlFor={id} label={label} description={description} descriptionId={descriptionId} />
      <Input
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        list={list}
        autoComplete="off"
        aria-describedby={error ? `${descriptionId} ${errorId}` : descriptionId}
        aria-invalid={Boolean(error) || undefined}
        data-workflow-setup-invalid={error ? "true" : undefined}
      />
      {error && (
        <p id={errorId} className="text-xs text-red-500">
          {error}
        </p>
      )}
    </div>
  )
}

function SwitchField({
  checked,
  description,
  id,
  label,
  onCheckedChange,
}: {
  checked: boolean
  description: string
  id: string
  label: string
  onCheckedChange: (checked: boolean) => void
}) {
  return (
    <div className="flex items-start justify-between gap-4 rounded-md border border-border p-4">
      <div className="min-w-0">
        <Label htmlFor={id}>{label}</Label>
        <p className="mt-1 text-xs text-muted-foreground">{description}</p>
      </div>
      <Switch id={id} checked={checked} onCheckedChange={onCheckedChange} />
    </div>
  )
}

function ConcurrencyField({
  defaultValue,
  onChange,
  options,
  value,
}: {
  defaultValue: string
  onChange: (value: string) => void
  options: string[]
  value: string
}) {
  return (
    <>
      <Datalist id="workflow-setup-concurrency-groups" values={options} />
      <TextField
        id="workflow-setup-concurrency-group"
        label="Concurrency group"
        description="Use an existing group, or leave blank to use the backend default."
        value={value}
        onChange={onChange}
        placeholder={defaultValue}
        list="workflow-setup-concurrency-groups"
      />
    </>
  )
}

function Notice({
  children,
  icon: Icon,
  tone,
}: {
  children: ReactNode
  icon: ElementType
  tone: "critical" | "warning" | "info" | "success"
}) {
  return (
    <div
      className={cn(
        "flex items-start gap-2 rounded-md border px-3 py-2 text-sm",
        tone === "critical" && "border-red-500/20 bg-red-500/10 text-red-600 dark:text-red-400",
        tone === "warning" && "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300",
        tone === "info" && "border-border bg-muted/40 text-muted-foreground",
        tone === "success" && "border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
      )}
    >
      <Icon className="mt-0.5 size-4 shrink-0" />
      <span>{children}</span>
    </div>
  )
}

function StatusBadge({ status }: { status: RequirementStatus }) {
  if (status === "available") {
    return (
      <Badge variant="secondary" className="gap-1">
        <CheckCircle2 className="size-3" />
        Available
      </Badge>
    )
  }
  if (status === "missing") {
    return (
      <Badge variant="destructive" className="gap-1">
        <XCircle className="size-3" />
        Missing
      </Badge>
    )
  }
  return (
    <Badge variant="outline" className="gap-1">
      <Lock className="size-3" />
      Not used
    </Badge>
  )
}

function SeverityIcon({ severity }: { severity: Diagnostic["severity"] }) {
  if (severity === "critical") return <AlertTriangle className="size-4 text-red-500" />
  if (severity === "warning") return <AlertTriangle className="size-4 text-amber-500" />
  return <Info className="size-4 text-muted-foreground" />
}

function Datalist({ id, values }: { id: string; values: string[] }) {
  if (values.length === 0) return null
  return (
    <datalist id={id}>
      {values.map((value) => (
        <option key={value} value={value} />
      ))}
    </datalist>
  )
}

function buildRenderInput(pattern: PatternMetadata, draft: WorkflowSetupDraft): RenderInput {
  const config = cloneDefaults(pattern.defaults)
  const concurrencyGroup = draft.concurrencyGroup.trim()
  if (concurrencyGroup) config.concurrencyGroup = concurrencyGroup
  if (draft.includePreCommit !== undefined) config.includePreCommit = draft.includePreCommit
  if (draft.preCommitCommand.trim()) config.preCommitCommand = draft.preCommitCommand.trim()

  switch (pattern.id) {
    case "github-issue": {
      const repository = draft.repository.trim()
      const triggerLabel = draft.triggerLabel.trim()
      if (repository) config.repository = repository
      if (triggerLabel) {
        config.triggerLabel = triggerLabel
        config.labels = [triggerLabel]
      }
      if (draft.enableManualTrigger !== undefined) config.enableManualTrigger = draft.enableManualTrigger
      if (draft.workingStatus.trim()) config.workingLabel = draft.workingStatus.trim()
      if (draft.prOpenedStatus.trim()) config.reviewLabel = draft.prOpenedStatus.trim()
      if (draft.mergedStatus.trim()) config.doneLabel = draft.mergedStatus.trim()
      if (draft.closedNoMergeStatus.trim()) config.closedLabel = draft.closedNoMergeStatus.trim()
      break
    }
    case "linear-status":
    case "shortcut-status": {
      if (draft.trackerWorkspace.trim()) config.workspace = draft.trackerWorkspace.trim()
      if (draft.triggerStatus.trim()) config.triggerStatus = draft.triggerStatus.trim()
      if (draft.workingStatus.trim()) config.workingStatus = draft.workingStatus.trim()
      if (draft.prOpenedStatus.trim()) config.prOpenedStatus = draft.prOpenedStatus.trim()
      if (draft.mergedStatus.trim()) config.mergedStatus = draft.mergedStatus.trim()
      if (draft.closedNoMergeStatus.trim()) config.closedNoMergeStatus = draft.closedNoMergeStatus.trim()
      break
    }
    case "manual-task": {
      const defaultInput = firstManualInput(pattern)
      const input: ManualInputConfig = {
        name: draft.manualInputName.trim() || stringFromUnknown(defaultInput.name),
        type: draft.manualInputType.trim() || stringFromUnknown(defaultInput.type),
        required: draft.manualInputRequired ?? booleanFromUnknown(defaultInput.required, false),
      }
      const description = draft.manualInputDescription.trim() || stringFromUnknown(defaultInput.description)
      const defaultValue = draft.manualInputDefault.trim() || stringFromUnknown(defaultInput.default)
      if (description) input.description = description
      if (defaultValue) input.default = defaultValue
      config.inputs = [input]
      if (draft.doneSignal.trim()) config.doneSignal = draft.doneSignal.trim()
      break
    }
  }

  return {
    workflowName: draft.workflowName.trim(),
    patternId: pattern.id,
    config,
  }
}

function getRenderReadiness(
  pattern: PatternMetadata | undefined,
  draft: WorkflowSetupDraft,
  renderInput: RenderInput | null
): { ready: boolean; reason: string } {
  if (!pattern || !renderInput) return { ready: false, reason: "Select a pattern to render workflow YAML." }
  if (!draft.workspaceName.trim()) return { ready: false, reason: "Select a workspace before rendering." }
  if (!draft.workflowName.trim()) return { ready: false, reason: "Enter a workflow name before rendering." }
  if (pattern.id === "github-issue" && !stringFromConfig(renderInput.config, "repository")) {
    return { ready: false, reason: "Choose a GitHub repository before rendering." }
  }
  if ((pattern.id === "linear-status" || pattern.id === "shortcut-status") && !stringFromConfig(renderInput.config, "workspace")) {
    return { ready: false, reason: "Choose an issue tracker workspace before rendering." }
  }
  if (pattern.id === "manual-task") {
    const input = Array.isArray(renderInput.config.inputs) ? renderInput.config.inputs[0] : null
    if (!isRecord(input) || !stringFromUnknown(input.name)) {
      return { ready: false, reason: "Configure a manual input before rendering." }
    }
  }
  return { ready: true, reason: "Ready to render and validate." }
}

function buildAccessRequirements(
  pattern: PatternMetadata | undefined,
  draft: WorkflowSetupDraft,
  context: SetupContext | null,
  contextReady: boolean
): AccessRequirement[] {
  if (!pattern || !context || !contextReady) return []

  const providerReady = context.readiness.providerReady
  const llmKeys = arrayValue(context.readiness.llmKeys)
  const modelReady = context.readiness.modelReady && llmKeys.some((key) => key.keySet)
  const requirements: AccessRequirement[] = [
    {
      id: "runtime-provider",
      label: "Runtime provider",
      description: "Workflow-created claws need an execution provider.",
      detail: providerReady
        ? providerDetail(context)
        : "No supported provider with credentials is ready.",
      status: providerReady ? "available" : "missing",
      settingsHref: SETTINGS_SECTIONS.runtimes,
      settingsLabel: "Open runtimes",
    },
    {
      id: "model",
      label: "Model",
      description: "Workflow-created claws need a configured model and key.",
      detail: modelReady
        ? modelDetail(context)
        : "No default model and configured LLM key are both ready.",
      status: modelReady ? "available" : "missing",
      settingsHref: SETTINGS_SECTIONS.models,
      settingsLabel: "Open models",
    },
  ]

  if (pattern.id === "github-issue") {
    const selectedRepo = draft.repository.trim()
    const repositories = arrayValue(context.workspace.repositories).map((repository) => repository.repo)
    const repoAvailable =
      repositories.length > 0 && (selectedRepo === "" || repositories.includes(selectedRepo))
    const tracker = findBestIssueTracker(context, "github-issues", "")
    requirements.push(
      {
        id: "github-repository",
        label: "Repository access",
        description: "GitHub issue workflows require a repository in the current workspace.",
        detail: repositories.length > 0 ? repositories.join(", ") : "No repositories are configured for this workspace.",
        status: repoAvailable ? "available" : "missing",
        settingsHref: SETTINGS_SECTIONS.github,
        settingsLabel: "Open GitHub",
      },
      {
        id: "github-issues-source",
        label: "GitHub Issues source",
        description: "Automatic issue triggers need GitHub Issues credentials and a webhook secret.",
        detail: issueTrackerDetail(tracker, "GitHub Issues"),
        status: tracker?.tokenSet && tracker.webhookSecretSet ? "available" : "missing",
        settingsHref: SETTINGS_SECTIONS.issueTrackers,
        settingsLabel: "Open issue trackers",
      }
    )
  } else if (pattern.id === "linear-status" || pattern.id === "shortcut-status") {
    const trackerType = pattern.id === "linear-status" ? "linear" : "shortcut"
    const trackerLabel = pattern.id === "linear-status" ? "Linear" : "Shortcut"
    const tracker = findBestIssueTracker(context, trackerType, draft.trackerWorkspace)
    requirements.push({
      id: `${trackerType}-source`,
      label: `${trackerLabel} issue source`,
      description: `${trackerLabel} status workflows need issue tracker credentials and a webhook secret.`,
      detail: issueTrackerDetail(tracker, trackerLabel),
      status: tracker?.tokenSet && tracker.webhookSecretSet ? "available" : "missing",
      settingsHref: SETTINGS_SECTIONS.issueTrackers,
      settingsLabel: "Open issue trackers",
    })
  } else {
    requirements.push({
      id: "manual-issue-source",
      label: "Issue source",
      description: "Manual task workflows do not subscribe to an issue tracker.",
      detail: "No issue tracker token or webhook secret is used by this pattern.",
      status: "not_used",
    })
  }

  const declaredSecretNames = arrayValue(context.workspace.declaredSecretNames)
  const secretNames = arrayValue(context.workspace.secretNames)
  const secretCount = declaredSecretNames.length + secretNames.length
  if (secretCount > 0) {
    requirements.push({
      id: "workspace-secrets",
      label: "Workspace secrets",
      description: "Secrets are referenced by name only and values are never shown here.",
      detail: `${secretNames.length} available, ${declaredSecretNames.length} declared.`,
      status: declaredSecretNames.every((secretName) => secretNames.includes(secretName))
        ? "available"
        : "missing",
      settingsHref: SETTINGS_SECTIONS.secrets,
      settingsLabel: "Open secrets",
    })
  }

  const defaultGroup = stringDefault(pattern.defaults, "concurrencyGroup")
  const group = draft.concurrencyGroup.trim() || defaultGroup
  if (group && group !== defaultGroup) {
    const found = arrayValue(context.concurrencyGroups).some((candidate) => candidate.name === group)
    requirements.push({
      id: "concurrency-group",
      label: "Concurrency group",
      description: "Custom concurrency groups must exist before saving.",
      detail: found ? `${group} is configured.` : `${group} is not configured.`,
      status: found ? "available" : "missing",
      settingsHref: SETTINGS_SECTIONS.runtimes,
      settingsLabel: "Open runtimes",
    })
  } else {
    requirements.push({
      id: "concurrency-group",
      label: "Concurrency group",
      description: "The backend default group is used when no custom group is set.",
      detail: group ? `${group} is used by default.` : "No concurrency group is set.",
      status: group ? "available" : "not_used",
    })
  }

  return requirements
}

function getSaveBlockReason({
  hasCriticals,
  renderPending,
  renderReadiness,
  renderResponse,
  validatePending,
  validationMatchesCurrent,
}: {
  hasCriticals: boolean
  renderPending: boolean
  renderReadiness: { ready: boolean; reason: string }
  renderResponse: RenderResponse | null
  validatePending: boolean
  validationMatchesCurrent: boolean
}) {
  if (renderPending || validatePending) return "Save is disabled while render or validation is pending."
  if (!renderReadiness.ready) return renderReadiness.reason
  if (!renderResponse) return "Save is disabled until YAML has rendered."
  if (!validationMatchesCurrent) return "Save is disabled until the current config hash matches the latest validation."
  if (hasCriticals) return "Resolve critical diagnostics before saving."
  return ""
}

function getFooterStatus({
  hasWarnings,
  saveBlockReason,
  saveError,
  savePending,
  savedResponse,
  warningSaveConfirmed,
}: {
  hasWarnings: boolean
  saveBlockReason: string
  saveError: string
  savePending: boolean
  savedResponse: SaveResponse | null
  warningSaveConfirmed: boolean
}) {
  if (savePending) return "Saving workflow..."
  if (saveError) return saveError
  if (savedResponse) {
    return isSavedWorkflowReadyToRun(savedResponse) ? "Saved. Ready to run." : "Saved as draft."
  }
  if (saveBlockReason) return saveBlockReason
  if (hasWarnings) {
    return warningSaveConfirmed ? "Warnings confirmed. Save with warnings is available." : "Review and confirm warnings before saving."
  }
  return "Ready to save."
}

function getLiveStatus({
  footerStatus,
  renderPending,
  validatePending,
  renderError,
  validateError,
  savedResponse,
  savePending,
}: {
  footerStatus: string
  renderPending: boolean
  validatePending: boolean
  renderError: string
  validateError: string
  savedResponse: SaveResponse | null
  savePending: boolean
}) {
  if (savePending) return "Saving workflow."
  if (savedResponse) return isSavedWorkflowReadyToRun(savedResponse) ? "Workflow saved and ready to run." : "Workflow saved as draft."
  if (renderPending) return "Rendering workflow YAML."
  if (validatePending) return "Validating workflow YAML."
  if (renderError) return `Render failed. ${renderError}`
  if (validateError) return `Validation failed. ${validateError}`
  return footerStatus
}

function getValidationStatus({
  currentConfigHash,
  renderError,
  renderPending,
  renderReadiness,
  validateError,
  validatePending,
  validateResponse,
  validationMatchesCurrent,
}: {
  currentConfigHash: string
  renderError: string
  renderPending: boolean
  renderReadiness: { ready: boolean; reason: string }
  validateError: string
  validatePending: boolean
  validateResponse: ValidateResponse | null
  validationMatchesCurrent: boolean
}) {
  if (!renderReadiness.ready) return renderReadiness.reason
  if (renderPending) return "Rendering YAML from the latest pattern config."
  if (validatePending) return "Validating the rendered YAML against workspace readiness."
  if (renderError) return "Render failed; update fields and try again."
  if (validateError) return "Validation failed; update fields and try again."
  if (!validateResponse) return "Validation has not run for this draft yet."
  if (currentConfigHash && !validationMatchesCurrent) return "Rendered YAML has changed since the last validation."
  if (validateResponse.summary.critical > 0) return "Critical diagnostics must be fixed before saving."
  if (validateResponse.summary.warning > 0) return "Warnings require explicit Save with warnings confirmation."
  return "Latest rendered config hash matches validation."
}

function getDraftFieldErrors(
  pattern: PatternMetadata | undefined,
  draft: WorkflowSetupDraft,
  renderInput: RenderInput | null
): DraftFieldErrors {
  const errors: DraftFieldErrors = {}
  if (!draft.workflowName.trim()) {
    errors.workflowName = "Workflow name is required."
  }
  if (!pattern || !renderInput) return errors

  if (pattern.id === "github-issue" && !stringFromConfig(renderInput.config, "repository")) {
    errors.repository = "Repository is required."
  }
  if ((pattern.id === "linear-status" || pattern.id === "shortcut-status") && !stringFromConfig(renderInput.config, "workspace")) {
    errors.trackerWorkspace = "Issue tracker workspace is required."
  }
  if (pattern.id === "manual-task") {
    const input = Array.isArray(renderInput.config.inputs) ? renderInput.config.inputs[0] : null
    if (!isRecord(input) || !stringFromUnknown(input.name)) {
      errors.manualInputName = "Manual input name is required."
    }
  }
  return errors
}

function firstDraftErrorStep(errors: DraftFieldErrors): StepId | null {
  if (errors.workflowName) return "pattern"
  if (errors.repository || errors.trackerWorkspace || errors.manualInputName) return "trigger"
  return null
}

function isSavedWorkflowReadyToRun(response: SaveResponse): boolean {
  return response.saved &&
    response.readiness.summary.critical === 0 &&
    response.readiness.summary.warning === 0 &&
    response.workflow.enabled
}

function manualTriggerInputsForWorkflow(workflow: SavedWorkflow): WorkflowInput[] {
  const inputs = workflow.inputs ?? []
  if (!isGitHubIssueManualWorkflow(workflow) || inputs.some((input) => input.name === "issue_number")) {
    return inputs
  }
  return [
    {
      name: "issue_number",
      type: "number",
      required: true,
      description: "GitHub issue number to run manually.",
      min: 1,
    },
    ...inputs,
  ]
}

function isGitHubIssueManualWorkflow(workflow: SavedWorkflow): boolean {
  return workflow.integration === "github-issues" || Boolean(workflow.inputs?.some((input) => input.name === "issue_number"))
}

function buildManualTriggerInputs(
  workflow: SavedWorkflow,
  inputs: WorkflowInput[],
  values: Record<string, string>
): { ok: true; inputs: Record<string, unknown> } | { ok: false; errors: Record<string, string> } {
  const result: Record<string, unknown> = {}
  const errors: Record<string, string> = {}
  const githubIssueManual = isGitHubIssueManualWorkflow(workflow)

  for (const input of inputs) {
    const raw = values[input.name] ?? ""
    const required = input.required || (githubIssueManual && input.name === "issue_number")
    if (required && raw.trim() === "") {
      errors[input.name] = input.name === "issue_number" ? "Issue number is required." : "This input is required."
      continue
    }
    if (raw.trim() === "" && !required) continue

    if (input.type === "bool") {
      result[input.name] = raw === "true"
      continue
    }
    if (input.type === "number" || input.name === "issue_number") {
      const value = Number(raw)
      const min = input.name === "issue_number" ? 1 : input.min
      if (!Number.isFinite(value)) {
        errors[input.name] = "Enter a valid number."
        continue
      }
      if (input.name === "issue_number" && !Number.isInteger(value)) {
        errors[input.name] = "Enter a whole issue number."
        continue
      }
      if (min !== undefined && value < min) {
        errors[input.name] = input.name === "issue_number" ? "Enter issue number 1 or higher." : `Enter ${min} or higher.`
        continue
      }
      if (input.max !== undefined && value > input.max) {
        errors[input.name] = `Enter ${input.max} or lower.`
        continue
      }
      result[input.name] = value
      continue
    }
    result[input.name] = raw
  }

  if (Object.keys(errors).length > 0) {
    return { ok: false, errors }
  }
  return { ok: true, inputs: result }
}

function sortDiagnostics(diagnostics: Diagnostic[]): Diagnostic[] {
  return [...diagnostics].sort((a, b) => {
    const severity = DIAGNOSTIC_RANK[a.severity] - DIAGNOSTIC_RANK[b.severity]
    if (severity !== 0) return severity
    return a.title.localeCompare(b.title)
  })
}

function groupDiagnostics(diagnostics: Diagnostic[]): Record<Diagnostic["severity"], Diagnostic[]> {
  return {
    critical: diagnostics.filter((diagnostic) => diagnostic.severity === "critical"),
    warning: diagnostics.filter((diagnostic) => diagnostic.severity === "warning"),
    info: diagnostics.filter((diagnostic) => diagnostic.severity === "info"),
  }
}

function settingsLinkForDiagnostic(diagnostic: Diagnostic): { href: string; label: string } | null {
  const haystack = [
    diagnostic.id,
    diagnostic.category,
    diagnostic.fieldPath,
    diagnostic.fixTarget,
    diagnostic.title,
    diagnostic.detail,
  ]
    .join(" ")
    .toLowerCase()

  if (haystack.includes("provider") || haystack.includes("runtime") || haystack.includes("concurrency")) {
    return { href: SETTINGS_SECTIONS.runtimes, label: "Open runtimes" }
  }
  if (haystack.includes("model") || haystack.includes("llm")) {
    return { href: SETTINGS_SECTIONS.models, label: "Open models" }
  }
  if (haystack.includes("github app") || haystack.includes("githubapps")) {
    return { href: SETTINGS_SECTIONS.github, label: "Open GitHub" }
  }
  if (haystack.includes("issue-tracker") || haystack.includes("issue tracker") || haystack.includes("webhook")) {
    return { href: SETTINGS_SECTIONS.issueTrackers, label: "Open issue trackers" }
  }
  if (haystack.includes("secret")) {
    return { href: SETTINGS_SECTIONS.secrets, label: "Open secrets" }
  }
  return null
}

function cloneDefaults(defaults: Record<string, unknown>): Record<string, unknown> {
  return JSON.parse(JSON.stringify(defaults)) as Record<string, unknown>
}

function firstManualInput(pattern: PatternMetadata): ManualInputConfig {
  const inputs = pattern.defaults.inputs
  if (!Array.isArray(inputs)) return {}
  const first = inputs[0]
  return isRecord(first) ? first : {}
}

function manualInputDefault(pattern: PatternMetadata, key: keyof ManualInputConfig, fallback: string): string
function manualInputDefault(pattern: PatternMetadata, key: keyof ManualInputConfig, fallback: boolean): boolean
function manualInputDefault(pattern: PatternMetadata, key: keyof ManualInputConfig, fallback: string | boolean): string | boolean {
  const value = firstManualInput(pattern)[key]
  if (typeof fallback === "boolean") return booleanFromUnknown(value, fallback)
  return stringFromUnknown(value) || fallback
}

function stringDefault(defaults: Record<string, unknown>, key: string, fallback = ""): string {
  return stringFromUnknown(defaults[key]) || fallback
}

function booleanDefault(defaults: Record<string, unknown>, key: string, fallback: boolean): boolean {
  return booleanFromUnknown(defaults[key], fallback)
}

function stringFromConfig(config: Record<string, unknown>, key: string): string {
  return stringFromUnknown(config[key])
}

function stringFromUnknown(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function optionalBooleanValue(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined
}

function booleanFromUnknown(value: unknown, fallback: boolean): boolean {
  return typeof value === "boolean" ? value : fallback
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function issueTrackerWorkspaces(context: SetupContext | null, type: string): string[] {
  if (!context) return []
  const workspaces = new Set<string>()
  for (const tracker of issueTrackersForContext(context)) {
    if (tracker.type === type && tracker.workspace.trim()) workspaces.add(tracker.workspace)
  }
  return [...workspaces]
}

function findBestIssueTracker(context: SetupContext, type: string, workspace: string) {
  const trackers = issueTrackersForContext(context).filter((tracker) => tracker.type === type)
  const requested = workspace.trim()
  if (requested) {
    return trackers.find((tracker) => tracker.workspace === requested)
  }
  return trackers[0]
}

function issueTrackersForContext(context: SetupContext): SetupIssueTrackerRef[] {
  return [
    ...arrayValue(context.workspace?.issueTrackers),
    ...arrayValue(context.hub?.issueTrackers),
  ]
}

function issueTrackerDetail(
  tracker: ReturnType<typeof findBestIssueTracker> | undefined,
  label: string
): string {
  if (!tracker) return `${label} tracker is not configured.`
  const token = tracker.tokenSet ? "token set" : "token missing"
  const webhook = tracker.webhookSecretSet ? "webhook secret set" : "webhook secret missing"
  return `${tracker.workspace || "default"}: ${token}, ${webhook}.`
}

function providerDetail(context: SetupContext): string {
  const defaultProvider = context.readiness.defaultProvider
  if (defaultProvider) return `${defaultProvider} is the default provider.`
  const providers = arrayValue(context.readiness.providers)
  return `${providers.length} provider${providers.length === 1 ? "" : "s"} configured.`
}

function modelDetail(context: SetupContext): string {
  const defaultModel = context.readiness.defaultModel
  if (defaultModel) return `${defaultModel} is the default model.`
  const configuredKeys = arrayValue(context.readiness.llmKeys).filter((key) => key.keySet).length
  return `${configuredKeys} configured LLM key${configuredKeys === 1 ? "" : "s"}.`
}

function arrayValue<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : []
}
