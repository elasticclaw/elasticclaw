"use client"

import { useId, useState, type MouseEvent } from "react"
import {
  Bot,
  Check,
  ChevronRight,
  CircleAlert,
  Eye,
  Globe,
  Search,
  SquarePen,
  Terminal,
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

export const CATEGORY_ICONS: Record<ToolCategory, typeof Wrench> = {
  read: Eye,
  edit: SquarePen,
  run: Terminal,
  search: Search,
  web: Globe,
  task: Bot,
  other: Wrench,
}

/** Shared by step rows, group rows, the activity summary and the turn fold. */
export const ROW_INTERACTIVE_CLASS =
  "cursor-pointer hover:bg-accent/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-focus-ring"

/** Expanded tool output: one quiet box. A failed step only adds a 2px `border-error/40` left rule; its text keeps the same quiet secondary tone. */
export const EXPANDED_BODY_CLASS = "mt-1 ms-7 cursor-default rounded-md bg-muted/40 px-3 py-2"
export const EXPANDED_PRE_CLASS =
  "overflow-auto whitespace-pre-wrap break-words font-mono leading-relaxed select-text cursor-text"

function StepIcon({ step, className }: { step: Step; className: string }) {
  if (step.status === "failed" || step.tone !== "normal") return <CircleAlert className={className} aria-hidden />
  if (step.kind === "info") return <Check className={className} aria-hidden />
  const Icon = CATEGORY_ICONS[step.category] ?? Wrench
  return <Icon className={className} aria-hidden />
}

/**
 * Failures of ordinary tool calls stay quiet — the icon turns into a muted
 * alert glyph and the row keeps its secondary text. Only session-level errors (info steps with an error
 * tone) and warnings get a colored heading.
 */
function stepTones(step: Step): { icon: string; title: string } {
  const severe = step.kind === "info" && step.tone === "error"
  if (severe) return { icon: "text-error-foreground", title: "font-medium text-error-foreground" }
  if (step.tone === "warning") return { icon: "text-warning", title: "font-medium text-warning" }
  if (step.status === "failed") return { icon: "text-tool-error-icon/70", title: "text-secondary-label" }
  return { icon: "text-icon-muted", title: "text-secondary-label" }
}

function stopRowToggle(e: MouseEvent) {
  e.stopPropagation()
}

/**
 * One tool call as a quiet 24px row: category icon, title + detail, duration,
 * chevron. Click expands the result/error inline; failed steps start expanded.
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
  // the dimmed icon + exit code already mark the failure there.
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
  const showDuration = duration !== undefined && duration >= (running ? 0 : 50)
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
  const tones = stepTones(step)
  const failed = step.status === "failed"
  // The status text often restates the detail ("waiting for <model>" next to
  // the model name); only show it when it adds something.
  const statusText = step.statusText?.trim() ?? ""
  const showStatusText =
    !isCard &&
    statusText.length > 0 &&
    !(step.detail && statusText.toLowerCase().includes(step.detail.trim().toLowerCase()))

  const bodyId = useId()
  const Header = interactive ? "button" : "div"

  return (
    <div className={cn("flex flex-col", expanded && "mb-1")}>
      <Header
        type={interactive ? "button" : undefined}
        aria-expanded={hasBody && !opensSubagent ? expanded : undefined}
        aria-controls={expanded && hasBody && !opensSubagent ? bodyId : undefined}
        aria-label={opensSubagent ? `Open subagent ${step.detail || step.title}` : undefined}
        onClick={activate ? (e) => activate(e.currentTarget) : undefined}
        className={cn(
          "flex w-full select-none items-center gap-1.5 rounded-md px-0.5 py-0.5 text-left transition-colors",
          isCard ? "min-h-5" : "min-h-6",
          // 44px tap target for expandable rows on touch screens
          interactive && (isCard ? "max-md:min-h-9" : "max-md:min-h-11"),
          interactive && ROW_INTERACTIVE_CLASS
        )}
      >
        <span className={cn("flex shrink-0 items-center justify-center", isCard ? "size-5" : "size-6", tones.icon)}>
          <StepIcon step={step} className={cn("shrink-0 stroke-[1.8]", isCard ? "size-3.5" : "size-4")} />
        </span>
        <span className={cn("flex min-w-0 flex-1 items-baseline gap-1.5 leading-relaxed", isCard ? "text-xs" : "text-sm")}>
          {/* Title + detail share one span so a running row shimmers as a whole;
              live-tool-shine's `color: transparent !important` wins over the
              tone classes, which stay on so reduced-motion keeps the quiet tone. */}
          <span
            className={cn(
              "flex min-w-0 flex-1 items-baseline gap-1.5 max-md:flex-wrap max-md:gap-y-0",
              running && "live-tool-shine"
            )}
          >
            <span className={cn("shrink-0", tones.title)}>
              {failed && <span className="sr-only">failed </span>}
              {step.title}
            </span>
            {step.detail && (
              <span
                className={cn(
                  // Mobile: wrap to two lines with break-all so long paths never
                  // force horizontal overflow; desktop keeps the one-line truncate.
                  "min-w-0 flex-1 truncate text-secondary-label max-md:line-clamp-2 max-md:whitespace-normal max-md:break-all",
                  step.detailKind !== "text" && (isCard ? "font-mono text-[11px]" : "font-mono text-[13px]")
                )}
                title={step.detail}
              >
                {step.detail}
              </span>
            )}
          </span>
          {showStatusText && (
            <span className="min-w-0 truncate text-xs text-muted-foreground max-md:hidden">{statusText}</span>
          )}
        </span>
        {showExit && (
          <span className="shrink-0 rounded-sm border border-border/60 px-1 font-mono text-[.65rem] text-muted-foreground">
            exit {step.exitCode}
          </span>
        )}
        {showDuration && (
          <span
            className="shrink-0 font-mono text-[.7rem] tabular-nums text-muted-foreground"
            suppressHydrationWarning={running || undefined}
          >
            {formatDurationMs(duration)}
          </span>
        )}
        <span className={cn("flex size-4 shrink-0 items-center justify-center", !interactive && "invisible")} aria-hidden>
          <ChevronRight
            className={cn(
              "size-3 shrink-0 text-icon-muted opacity-70 transition-transform duration-200",
              expanded && "rotate-90"
            )}
          />
        </span>
      </Header>
      {expanded && (
        <div
          id={bodyId}
          className={cn(EXPANDED_BODY_CLASS, "flex flex-col gap-2", failed && "border-l-2 border-error/40")}
          onClick={stopRowToggle}
        >
          {step.error && (
            <pre
              className={cn(
                EXPANDED_PRE_CLASS,
                "text-secondary-label",
                isCard ? "max-h-32 text-[10px]" : "max-h-64 text-[11px]"
              )}
            >
              {step.error}
            </pre>
          )}
          {step.result && (
            <pre
              className={cn(
                EXPANDED_PRE_CLASS,
                "text-secondary-label",
                isCard ? "max-h-32 text-[10px]" : "max-h-64 text-[11px]"
              )}
            >
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
    <div className="flex flex-col">
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

/** Past this many rows the expanded group scrolls, so it gets the edge fade. */
const GROUP_SCROLL_THRESHOLD = 10

/** Each edge fades only while rows are hidden past it, so the first and last rows read fully opaque. */
const GROUP_EDGE_FADE = cn(
  "[--fade-top:black] [--fade-bottom:black]",
  "data-[fade-top=true]:[--fade-top:transparent] data-[fade-bottom=true]:[--fade-bottom:transparent]",
  "[mask-image:linear-gradient(to_bottom,var(--fade-top),black_1.5rem,black_calc(100%-1.5rem),var(--fade-bottom))]"
)

function syncEdgeFade(el: HTMLElement | null) {
  if (!el) return
  el.dataset.fadeTop = String(el.scrollTop > 0)
  el.dataset.fadeBottom = String(el.scrollHeight - el.scrollTop - el.clientHeight > 1)
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
    <div key={id} className="flex flex-col">
      <button
        type="button"
        aria-expanded={expanded}
        onClick={(e) => {
          anchor(e.currentTarget)
          setExpanded((v) => !v)
        }}
        className={cn(
          "flex w-full items-center gap-1.5 rounded-md px-0.5 py-0.5 text-left leading-relaxed transition-colors",
          isCard ? "min-h-5 text-xs" : "min-h-6 text-sm",
          isCard ? "max-md:min-h-9" : "max-md:min-h-11",
          ROW_INTERACTIVE_CLASS
        )}
      >
        <span className={cn("flex shrink-0 items-center justify-center text-icon-muted", isCard ? "size-5" : "size-6")}>
          <Icon className={cn("shrink-0 stroke-[1.8]", isCard ? "size-3.5" : "size-4")} aria-hidden />
        </span>
        <span className="min-w-0 flex-1 truncate text-secondary-label">{label}</span>
        {totalMs > 0 && (
          <span className="shrink-0 font-mono text-[.7rem] tabular-nums text-muted-foreground">{formatDurationMs(totalMs)}</span>
        )}
        <span className="flex size-4 shrink-0 items-center justify-center" aria-hidden>
          <ChevronRight
            className={cn("size-3 shrink-0 text-icon-muted opacity-70 transition-transform duration-200", expanded && "rotate-90")}
          />
        </span>
      </button>
      {expanded && (
        <div
          // Inline ref: re-syncs on every render, so rows streaming in update the bottom fade.
          ref={(el) => syncEdgeFade(el)}
          onScroll={(e) => syncEdgeFade(e.currentTarget)}
          className={cn(
            "flex flex-col rounded-md",
            !isCard && "max-h-[min(18rem,50dvh)] overflow-y-auto scrollbar-thin",
            !isCard && steps.length > GROUP_SCROLL_THRESHOLD && GROUP_EDGE_FADE
          )}
        >
          {steps.map((step) => (
            <StepRow key={step.id} step={step} density={density} now={now} onOpenSubagent={onOpenSubagent} />
          ))}
        </div>
      )}
    </div>
  )
}
