"use client"

import { useState } from "react"
import {
  AlertCircle,
  Bot,
  ChevronRight,
  FileText,
  Globe,
  Info,
  Loader2,
  Search,
  SquarePen,
  SquareTerminal,
  Wrench,
} from "lucide-react"
import { cn } from "@/lib/utils"
import { isSubagentStep } from "@/lib/subagents"
import {
  formatDurationMs,
  type Step,
  type StepListItem,
  type ToolCategory,
} from "@/lib/turns"
import { useToggleAnchor } from "./anchor-context"

export type StepDensity = "full" | "card"

const CATEGORY_ICONS: Record<ToolCategory, typeof Wrench> = {
  read: FileText,
  edit: SquarePen,
  run: SquareTerminal,
  search: Search,
  web: Globe,
  task: Bot,
  other: Wrench,
}

function StepIcon({ step, className }: { step: Step; className: string }) {
  if (step.status === "running") return <Loader2 className={cn(className, "animate-spin")} />
  if (step.status === "failed" || step.tone !== "normal") return <AlertCircle className={className} />
  if (step.kind === "info") return <Info className={className} />
  const Icon = CATEGORY_ICONS[step.category] ?? Wrench
  return <Icon className={className} />
}

function stepAccent(step: Step): string {
  if (step.status === "running") return "border-l-blue-500"
  if (step.status === "failed") return "border-l-red-500"
  if (step.tone === "warning") return "border-l-amber-500/70"
  return "border-l-transparent"
}

function stepTextTone(step: Step): string {
  if (step.status === "failed" || step.tone === "error") return "text-red-400"
  if (step.tone === "warning") return "text-amber-400"
  return "text-foreground/80"
}

/**
 * One row per tool call: category icon / body / duration on a 3-column grid.
 * Click expands the result/error inline; failed steps start expanded.
 */
export function StepRow({
  step,
  density = "full",
  now,
  onOpenSubagent,
}: {
  step: Step
  density?: StepDensity
  /** Live clock (ms) — pass while the step is running so elapsed time ticks. */
  now?: number
  /**
   * Opt-in: when provided, a Task step becomes a link into the subagent
   * drill-down instead of expanding its result inline. Only the chat view
   * passes it — board cards and the run-logs dialog have nowhere to drill to,
   * and keep the inline expansion untouched.
   */
  onOpenSubagent?: (stepId: string) => void
}) {
  // Failed steps auto-expand until the user explicitly toggles them — full
  // density only: a board card cannot afford an error dump eating its height,
  // the red row + exit code already mark the failure there.
  const [userToggled, setUserToggled] = useState<boolean | null>(null)
  const anchor = useToggleAnchor()
  const hasBody = Boolean(step.result || step.error)
  const isCard = density === "card"
  // isSubagentStep, not `category === "task"`: toolCategory maps Skill,
  // TaskStop, spawn_task and anything matching /workflow|dispatch/ to "task"
  // too, and those have no drill-down to open — they would trade their inline
  // result for a chevron that does nothing.
  const opensSubagent = Boolean(onOpenSubagent) && isSubagentStep(step)
  const expanded = !opensSubagent && (userToggled ?? (step.status === "failed" && !isCard)) && hasBody

  const running = step.status === "running"
  const liveElapsed = running && now ? Math.max(0, now - step.startedAt.getTime()) : null
  const duration = liveElapsed ?? step.durationMs
  const showExit = typeof step.exitCode === "number" && step.exitCode !== 0

  const activate: ((el: HTMLElement) => void) | null = opensSubagent
    ? () => onOpenSubagent!(step.id)
    : hasBody
      ? (el) => {
          anchor(el)
          setUserToggled(!expanded)
        }
      : null
  const interactive = opensSubagent || hasBody

  return (
    <div className={cn("border-l-2 pl-2", stepAccent(step))}>
      <div
        role={interactive ? "button" : undefined}
        tabIndex={interactive ? 0 : undefined}
        aria-label={opensSubagent ? `Open subagent ${step.detail || step.title}` : undefined}
        onClick={
          activate
            ? (e) => activate(e.currentTarget as HTMLElement)
            : undefined
        }
        onKeyDown={
          activate
            ? (e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault()
                  activate(e.currentTarget as HTMLElement)
                }
              }
            : undefined
        }
        className={cn(
          "grid grid-cols-[auto_1fr_auto] items-baseline gap-x-2",
          isCard ? "py-0.5" : "py-1",
          // 44px tap target for expandable rows on touch screens
          interactive && "cursor-pointer rounded-sm hover:bg-muted/30 max-md:min-h-11 max-md:items-center",
          opensSubagent && "focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
        )}
      >
        <span className="self-center">
          <StepIcon step={step} className={cn(isCard ? "size-2.5" : "size-3", "shrink-0", stepTextTone(step))} />
        </span>
        <span className="flex min-w-0 items-baseline gap-1.5 max-md:flex-wrap max-md:gap-y-0">
          {running && (
            <span className="relative flex size-1.5 self-center">
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-blue-400 opacity-75" />
              <span className="relative inline-flex size-1.5 rounded-full bg-blue-500" />
            </span>
          )}
          <span className={cn("shrink-0 font-medium", isCard ? "text-[10px]" : "text-xs", stepTextTone(step))}>
            {step.title}
          </span>
          {step.detail && (
            <span
              className={cn(
                // Mobile: wrap to two lines with break-all so long paths never
                // force horizontal overflow; desktop keeps the one-line truncate.
                "min-w-0 truncate max-md:line-clamp-2 max-md:whitespace-normal max-md:break-all text-muted-foreground",
                step.detailKind !== "text" && "font-mono",
                isCard ? "text-[10px]" : "text-xs"
              )}
              title={step.detail}
            >
              {step.detail}
            </span>
          )}
          {step.statusText && !isCard && (
            <span className="min-w-0 truncate text-xs text-muted-foreground/50">{step.statusText}</span>
          )}
          {showExit && (
            <span className={cn("shrink-0 rounded bg-red-500/10 px-1 font-mono text-red-400", isCard ? "text-[9px]" : "text-[10px]")}>
              exit {step.exitCode}
            </span>
          )}
        </span>
        <span className="flex items-center gap-1 self-center">
          {duration !== undefined && duration >= (running ? 0 : 50) && (
            <span className="font-mono text-[10.5px] text-muted-foreground" suppressHydrationWarning={running || undefined}>
              {formatDurationMs(duration)}
            </span>
          )}
          {interactive && (
            <ChevronRight className={cn("size-3 text-muted-foreground/50 transition-transform", expanded && "rotate-90")} />
          )}
        </span>
      </div>
      {expanded && (
        <div className={cn("mb-1 space-y-1", isCard ? "pr-1" : "pr-2")}>
          {step.error && (
            <pre className={cn(
              "overflow-y-auto whitespace-pre-wrap break-words rounded border border-red-500/20 bg-red-500/5 p-2 font-mono text-red-400",
              isCard ? "max-h-32 text-[10px]" : "max-h-64 text-[11px]"
            )}>
              {step.error}
            </pre>
          )}
          {step.result && (
            <pre className={cn(
              "overflow-y-auto whitespace-pre-wrap break-words rounded border border-border/50 bg-muted/40 p-2 font-mono text-muted-foreground",
              isCard ? "max-h-32 text-[10px]" : "max-h-64 text-[11px]"
            )}>
              {step.result}
            </pre>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * Renders a step list with consecutive same-tool runs collapsed into one line
 * ("Read 7 files") that expands into the individual rows.
 */
export function StepList({
  items,
  density = "full",
  now,
  onOpenSubagent,
}: {
  items: StepListItem[]
  density?: StepDensity
  now?: number
  /** Opt-in Task-step drill-down; see StepRow. */
  onOpenSubagent?: (stepId: string) => void
}) {
  return (
    <div className={cn(density === "card" ? "space-y-0" : "space-y-0.5")}>
      {items.map((item) =>
        item.type === "step" ? (
          <StepRow key={item.step.id} step={item.step} density={density} now={now} onOpenSubagent={onOpenSubagent} />
        ) : (
          <StepGroupRow key={item.id} id={item.id} label={item.label} steps={item.steps} density={density} now={now} onOpenSubagent={onOpenSubagent} />
        )
      )}
    </div>
  )
}

function StepGroupRow({
  id,
  label,
  steps,
  density,
  now,
  onOpenSubagent,
}: {
  id: string
  label: string
  steps: Step[]
  density: StepDensity
  now?: number
  onOpenSubagent?: (stepId: string) => void
}) {
  const [expanded, setExpanded] = useState(false)
  const anchor = useToggleAnchor()
  const isCard = density === "card"
  const totalMs = steps.reduce((sum, s) => sum + (s.durationMs ?? 0), 0)
  const Icon = CATEGORY_ICONS[steps[0].category] ?? Wrench

  return (
    <div key={id} className="border-l-2 border-l-transparent pl-2">
      <button
        type="button"
        onClick={(e) => {
          anchor(e.currentTarget)
          setExpanded((v) => !v)
        }}
        className={cn(
          "grid w-full grid-cols-[auto_1fr_auto] items-center gap-x-2 rounded-sm text-left hover:bg-muted/30 max-md:min-h-11",
          isCard ? "py-0.5" : "py-1"
        )}
      >
        <Icon className={cn(isCard ? "size-2.5" : "size-3", "shrink-0 text-muted-foreground")} />
        <span className={cn("min-w-0 truncate font-medium text-muted-foreground", isCard ? "text-[10px]" : "text-xs")}>
          {label}
        </span>
        <span className="flex items-center gap-1">
          {totalMs > 0 && (
            <span className="font-mono text-[10.5px] text-muted-foreground">{formatDurationMs(totalMs)}</span>
          )}
          <ChevronRight className={cn("size-3 text-muted-foreground/50 transition-transform", expanded && "rotate-90")} />
        </span>
      </button>
      {expanded && (
        <div className={cn(density === "card" ? "space-y-0" : "space-y-0.5")}>
          {steps.map((step) => (
            <StepRow key={step.id} step={step} density={density} now={now} onOpenSubagent={onOpenSubagent} />
          ))}
        </div>
      )}
    </div>
  )
}
