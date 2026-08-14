// Pure timeline logic: pairing tool-call activities into steps, grouping the
// transcript into turns, and deriving labels/stats. No React in here — the web
// app has no test runner, so this file is the reviewable surface for the
// timeline behavior.

import type { ActivitySummary as ActivitySummaryMeta, Message } from "@/lib/types"

// ─── Activity helpers (moved from conversation-view.tsx) ─────────────────────

// Phase vocabularies mirror the bridge's isToolStartPhase/isToolTerminalPhase
// (cmd/claw-bridge/main.go) — older bridges forward gateway phases verbatim,
// so the web must accept the full set or start/terminal events stop pairing.
const START_PHASES = ["running", "start", "started", "in_progress"]
const TERMINAL_PHASES = ["completed", "complete", "done", "failed", "error", "cancelled", "canceled"]

export function isPhaseMessage(value?: string): boolean {
  if (!value) return false
  const lower = value.toLowerCase()
  return START_PHASES.includes(lower) || TERMINAL_PHASES.includes(lower)
}

export function nonPhaseMessage(value?: string): string {
  return isPhaseMessage(value) ? "" : value || ""
}

export function activityTitle(message: Message): string {
  const activity = message.activity
  if (!activity) return "Activity"
  if (activity.kind === "session_error") return "Session issue"
  if (activity.error) return "Agent error"
  if (activity.kind === "model_started") return "Waiting for model"
  if (activity.kind === "tool") return activity.tool || activity.phase || "Tool"
  if (activity.kind === "diagnostic") return "Diagnostic"
  return activity.tool || activity.phase || activity.stream || "Activity"
}

export function activityDetail(message: Message): string {
  const activity = message.activity
  if (!activity) return nonPhaseMessage(message.content)
  if (activity.kind === "model_started" && activity.message?.startsWith("waiting for ")) {
    return activity.message.replace(/^waiting for\s+/, "")
  }
  return activity.error || activity.command || activity.path || activity.url || activity.detail || nonPhaseMessage(activity.message) || nonPhaseMessage(message.content)
}

export type ActivityDetailKind = "command" | "path" | "url" | "text"

export function activityDetailKind(message: Message): ActivityDetailKind {
  const activity = message.activity
  if (activity?.command) return "command"
  if (activity?.path) return "path"
  if (activity?.url) return "url"
  return "text"
}

export function activityStatusText(message: Message): string {
  const activity = message.activity
  if (!activity?.message || isPhaseMessage(activity.message)) return ""
  const detail = activityDetail(message)
  return activity.message === detail ? "" : activity.message
}

export function isHiddenActivity(message: Message): boolean {
  return message.activity?.kind === "still_working" || message.content.startsWith("No streamed output")
}

export type ActivityTone = "error" | "warning" | "normal"

export function activityTone(message: Message): ActivityTone {
  if (message.activity?.error && message.activity.kind === "tool") return "error"
  if (message.activity?.error || message.activity?.kind === "session_error") return "warning"
  return "normal"
}

/** Content hash used to pair start/terminal events when the bridge emitted no call_id. */
export function activityGroupKey(message: Message): string {
  const activity = message.activity
  if (!activity) return message.id
  return [
    activity.kind || "",
    activity.tool || "",
    activity.detail || "",
    activity.command || "",
    activity.path || "",
    activity.url || "",
  ].join("\u0000")
}

export function isRunningActivity(message: Message): boolean {
  const phase = (message.activity?.phase || message.activity?.message || "").toLowerCase()
  return message.activity?.kind === "tool" && START_PHASES.includes(phase)
}

export function isTerminalActivity(message: Message): boolean {
  const phase = (message.activity?.phase || message.activity?.message || "").toLowerCase()
  return message.activity?.kind === "tool" && TERMINAL_PHASES.includes(phase)
}

/** Drop start events whose terminal twin is present (legacy row-level dedupe). */
export function coalesceActivityMessages(messages: Message[]): Message[] {
  const terminalKeys = new Set(messages.filter(isTerminalActivity).map(activityGroupKey))
  return messages.filter((message) => !(isRunningActivity(message) && terminalKeys.has(activityGroupKey(message))))
}

// ─── Steps: one row per tool call ────────────────────────────────────────────

export type StepStatus = "running" | "ok" | "failed"

export type ToolCategory = "read" | "edit" | "run" | "search" | "web" | "task" | "other"

export interface Step {
  id: string
  /** "tool" steps pair a start and a terminal event; "info" steps are single events (model wait, session errors, diagnostics). */
  kind: "tool" | "info"
  status: StepStatus
  /** Amber/red accent independent of pass/fail (session errors warn without failing the turn). */
  tone: ActivityTone
  category: ToolCategory
  title: string
  detail: string
  detailKind: ActivityDetailKind
  statusText: string
  startedAt: Date
  endedAt?: Date
  durationMs?: number
  exitCode?: number
  result?: string
  error?: string
  /** Underlying messages: start first, terminal last when present. */
  messages: Message[]
}

const READ_TOOLS = /read|cat|view|open|notebook/
const EDIT_TOOLS = /edit|write|patch|apply|create|multiedit/
const RUN_TOOLS = /bash|shell|exec|terminal|command|run/
const SEARCH_TOOLS = /grep|glob|search|find|ls|list/
const WEB_TOOLS = /web|fetch|http|browser|url|navigate/
const TASK_TOOLS = /task|agent|skill|workflow|dispatch/

export function toolCategory(message: Message): ToolCategory {
  const activity = message.activity
  const tool = (activity?.tool || "").toLowerCase()
  if (READ_TOOLS.test(tool)) return "read"
  if (EDIT_TOOLS.test(tool)) return "edit"
  if (RUN_TOOLS.test(tool)) return "run"
  if (SEARCH_TOOLS.test(tool)) return "search"
  if (WEB_TOOLS.test(tool)) return "web"
  if (TASK_TOOLS.test(tool)) return "task"
  if (activity?.command) return "run"
  if (activity?.url) return "web"
  if (activity?.path) return "read"
  return "other"
}

function terminalFailed(message: Message): boolean {
  const activity = message.activity
  if (!activity) return false
  const phase = (activity.phase || activity.message || "").toLowerCase()
  if (phase === "failed" || phase === "error") return true
  if (typeof activity.exit_code === "number" && activity.exit_code !== 0) return true
  return Boolean(activity.error)
}

function makeStep(message: Message, kind: Step["kind"], status: StepStatus): Step {
  const activity = message.activity
  return {
    id: message.id,
    kind,
    status,
    tone: activityTone(message),
    category: kind === "tool" ? toolCategory(message) : "other",
    title: activityTitle(message),
    detail: activityDetail(message),
    detailKind: activityDetailKind(message),
    statusText: activityStatusText(message),
    startedAt: message.timestamp,
    durationMs: activity?.duration_ms,
    exitCode: activity?.exit_code,
    result: activity?.result,
    error: activity?.error,
    messages: [message],
  }
}

function resolveStep(step: Step, terminal: Message): void {
  const activity = terminal.activity
  step.endedAt = terminal.timestamp
  step.status = terminalFailed(terminal) ? "failed" : "ok"
  step.tone = step.status === "failed" ? "error" : activityTone(terminal)
  step.durationMs = activity?.duration_ms ?? Math.max(0, terminal.timestamp.getTime() - step.startedAt.getTime())
  if (activity?.exit_code !== undefined) step.exitCode = activity.exit_code
  if (activity?.result) step.result = activity.result
  if (activity?.error) step.error = activity.error
  const statusText = activityStatusText(terminal)
  if (statusText) step.statusText = statusText
  step.messages.push(terminal)
}

/**
 * Pair tool start/terminal events into one Step per call. Pairs by
 * `activity.call_id` when present, falling back to the content hash
 * (`activityGroupKey`) for older bridges that emit no call_id.
 *
 * Steps left "running" here may be stale history — callers decide which
 * trailing steps are genuinely live via `demoteStaleRunning`.
 */
export function pairActivitySteps(activities: Message[]): Step[] {
  const steps: Step[] = []
  // key → stack of unresolved steps (repeated identical commands under the
  // hash fallback resolve most-recent-first).
  const open = new Map<string, Step[]>()

  for (const message of activities) {
    if (message.role !== "activity" || isHiddenActivity(message)) continue
    const activity = message.activity
    if (!activity || activity.kind !== "tool") {
      const step = makeStep(message, "info", activity?.kind === "model_started" ? "running" : "ok")
      if (step.tone === "error") step.status = "failed"
      steps.push(step)
      continue
    }
    const key = activity.call_id || activityGroupKey(message)
    if (isRunningActivity(message)) {
      // A call_id names one call: a second running event for an open call is a
      // progress pulse ("running for 2m"), not a new step. Without the merge
      // the pulse would both duplicate the row and steal the terminal event
      // (the stack pops most-recent-first), leaving the real start running
      // forever. The hash-fallback path keeps the stack semantics — there,
      // identical running events may genuinely be distinct calls.
      const openStack = open.get(key)
      if (activity.call_id && openStack && openStack.length > 0) {
        const step = openStack[openStack.length - 1]
        const statusText = activityStatusText(message)
        if (statusText) step.statusText = statusText
        step.messages.push(message)
        continue
      }
      const step = makeStep(message, "tool", "running")
      steps.push(step)
      if (openStack) openStack.push(step)
      else open.set(key, [step])
    } else if (isTerminalActivity(message)) {
      const started = open.get(key)?.pop()
      if (started) {
        resolveStep(started, message)
      } else {
        // Terminal without a visible start (start pruned or from an older page).
        const step = makeStep(message, "tool", terminalFailed(message) ? "failed" : "ok")
        step.endedAt = message.timestamp
        steps.push(step)
      }
    } else {
      steps.push(makeStep(message, "tool", "ok"))
    }
  }
  return steps
}

/**
 * History cannot still be running: demote unresolved "running" steps to "ok",
 * optionally keeping the chronologically last one live (the tail of a live
 * transcript).
 */
export function demoteStaleRunning(steps: Step[], keepTrailingRunning: boolean): Step[] {
  let lastRunningIdx = -1
  for (let i = steps.length - 1; i >= 0; i -= 1) {
    if (steps[i].status === "running") { lastRunningIdx = i; break }
  }
  if (lastRunningIdx === -1) return steps
  return steps.map((step, i) => {
    if (step.status !== "running") return step
    if (keepTrailingRunning && i === lastRunningIdx) return step
    return { ...step, status: "ok" as StepStatus }
  })
}

export function isProblemStep(step: Step): boolean {
  return step.status === "failed" || step.tone !== "normal"
}

// ─── Collapsing runs of same-tool steps ──────────────────────────────────────

export type StepListItem =
  | { type: "step"; step: Step }
  | { type: "group"; id: string; label: string; category: ToolCategory; steps: Step[] }

const MIN_GROUP_RUN = 3

function groupLabel(category: ToolCategory, title: string, count: number): string {
  switch (category) {
    case "read": return `Read ${count} files`
    case "edit": return `Edited ${count} files`
    case "run": return `Ran ${count} commands`
    case "search": return `${count} searches`
    case "web": return `Fetched ${count} URLs`
    default: return `${title} × ${count}`
  }
}

/**
 * Collapse consecutive same-tool, same-category successful steps into one
 * line ("Read 7 files"). Running/failed/toned steps always break the run so
 * problems and live work never hide inside a group.
 */
export function collapseStepRuns(steps: Step[]): StepListItem[] {
  const items: StepListItem[] = []
  let run: Step[] = []

  const flush = () => {
    if (run.length >= MIN_GROUP_RUN) {
      items.push({
        type: "group",
        id: `group-${run[0].id}`,
        label: groupLabel(run[0].category, run[0].title, run.length),
        category: run[0].category,
        steps: run,
      })
    } else {
      for (const step of run) items.push({ type: "step", step })
    }
    run = []
  }

  for (const step of steps) {
    const groupable = step.kind === "tool" && step.status === "ok" && step.tone === "normal"
    if (!groupable) {
      flush()
      items.push({ type: "step", step })
      continue
    }
    if (run.length > 0 && (run[0].title !== step.title || run[0].category !== step.category)) flush()
    run.push(step)
  }
  flush()
  return items
}

// ─── Turns: the unit of the timeline ─────────────────────────────────────────

export type TurnStatus = "running" | "ok" | "failed"

export type TurnItem =
  | { type: "message"; message: Message }
  | { type: "steps"; id: string; steps: Step[] }
  | { type: "summary"; message: Message }

export interface Turn {
  id: string
  index: number
  userMessage: Message | null
  items: TurnItem[]
  /** All steps flattened, in order (excludes lazy activity_summary contents). */
  steps: Step[]
  agentReplies: Message[]
  startedAt: Date
  endedAt: Date
  status: TurnStatus
  failedCount: number
  toolCallCount: number
  durationMs: number
  hasProblems: boolean
  summaryMeta?: ActivitySummaryMeta
}

/**
 * A turn starts at a user message (or at the start of the transcript) and
 * holds everything until the next user message: hub notices, tool steps, and
 * the agent's prose. Only the last turn may be "running".
 */
export function groupIntoTurns(messages: Message[]): Turn[] {
  const turns: Turn[] = []
  let current: Message[] = []
  let currentUser: Message | null = null

  const flushTurn = () => {
    if (!currentUser && current.length === 0) return
    turns.push(buildTurn(currentUser, current, turns.length))
    current = []
    currentUser = null
  }

  for (const message of messages) {
    if (message.role === "user") {
      flushTurn()
      currentUser = message
      continue
    }
    current.push(message)
  }
  flushTurn()

  // Only the trailing step of the final turn may still be running.
  for (let t = 0; t < turns.length; t += 1) {
    const isLast = t === turns.length - 1
    const turn = turns[t]
    const demoted = demoteStaleRunning(turn.steps, isLast)
    if (demoted !== turn.steps) {
      const byId = new Map(demoted.map((s) => [s.id, s]))
      turn.steps = demoted
      turn.items = turn.items.map((item) =>
        item.type === "steps"
          ? { ...item, steps: item.steps.map((s) => byId.get(s.id) ?? s) }
          : item
      )
    }
    turn.failedCount = turn.steps.filter((s) => s.status === "failed").length
    turn.hasProblems = turn.steps.some(isProblemStep)
    const running = isLast && turn.steps.some((s) => s.status === "running")
    turn.status = turn.failedCount > 0 ? "failed" : running ? "running" : "ok"
  }
  return turns
}

function buildTurn(userMessage: Message | null, rest: Message[], index: number): Turn {
  const items: TurnItem[] = []
  const agentReplies: Message[] = []
  let summaryMeta: ActivitySummaryMeta | undefined
  let summaryCount = 0
  let run: Message[] = []

  const flushRun = () => {
    if (run.length === 0) return
    const steps = pairActivitySteps(run)
    if (steps.length > 0) items.push({ type: "steps", id: `steps-${run[0].id}`, steps })
    run = []
  }

  for (const message of rest) {
    if (message.role === "activity") {
      run.push(message)
      continue
    }
    flushRun()
    if (message.role === "activity_summary") {
      if (!summaryMeta && message.activitySummary) summaryMeta = message.activitySummary
      summaryCount += message.activitySummary?.count ?? 0
      items.push({ type: "summary", message })
      continue
    }
    if (message.role === "claw") agentReplies.push(message)
    items.push({ type: "message", message })
  }
  flushRun()

  const steps = items.flatMap((item) => (item.type === "steps" ? item.steps : []))
  const first = userMessage ?? rest[0]
  const last = rest.length > 0 ? rest[rest.length - 1] : first
  const startedAt = first?.timestamp ?? new Date(0)
  const endedAt = last?.timestamp ?? startedAt

  return {
    id: `turn-${(userMessage ?? rest[0])?.id ?? index}`,
    index,
    userMessage,
    items,
    steps,
    agentReplies,
    startedAt,
    endedAt,
    status: "ok",
    failedCount: 0,
    toolCallCount: steps.filter((s) => s.kind === "tool").length + summaryCount,
    durationMs: Math.max(0, endedAt.getTime() - startedAt.getTime()),
    hasProblems: false,
    summaryMeta,
  }
}

// ─── Labels, stats, formatting ───────────────────────────────────────────────

type LabelCategory = "explore" | "edit" | "run" | "web" | "work"

const GENERIC_PATH_TOKENS = new Set([
  "src", "lib", "app", "web", "pkg", "cmd", "internal", "components", "hooks",
  "utils", "index", "main", "dist", "node", "modules", "test", "tests", "go",
  "ts", "tsx", "js", "jsx", "json", "md", "the", "and",
])

/** Most common path token across steps, if it appears in enough of them. */
export function dominantPathHint(steps: Step[]): string | null {
  const withPaths = steps.filter((s) => s.messages[0]?.activity?.path)
  if (withPaths.length < 2) return null
  const counts = new Map<string, number>()
  for (const step of withPaths) {
    const path = step.messages[0].activity?.path ?? ""
    const tokens = new Set(
      path.toLowerCase().split(/[/\\._\-\s]+/).filter((t) => t.length >= 3 && !GENERIC_PATH_TOKENS.has(t))
    )
    for (const token of tokens) counts.set(token, (counts.get(token) ?? 0) + 1)
  }
  let best: string | null = null
  let bestCount = 0
  for (const [token, count] of counts) {
    if (count > bestCount) { best = token; bestCount = count }
  }
  const threshold = Math.max(2, Math.ceil(withPaths.length * 0.6))
  return bestCount >= threshold ? best : null
}

const TESTY_COMMAND = /(^|[\s/])(test|vitest|jest|pytest|lint|tsc|typecheck|build|check)\b/i

function looksLikeTesting(steps: Step[]): boolean {
  return steps.some((s) => s.category === "run" && TESTY_COMMAND.test(s.messages[0]?.activity?.command ?? ""))
}

/** Short generated label from the dominant tool categories. Never calls a model. */
export function turnLabel(turn: Turn): string {
  const toolSteps = turn.steps.filter((s) => s.kind === "tool")
  if (toolSteps.length === 0 && turn.toolCallCount > 0) return "Ran tools"
  if (toolSteps.length === 0) {
    if (turn.status === "running") return "Working"
    if (turn.agentReplies.length > 0) return "Replied"
    return "No activity"
  }

  const counts = new Map<LabelCategory, number>()
  for (const step of toolSteps) {
    const cat: LabelCategory =
      step.category === "read" || step.category === "search" ? "explore"
      : step.category === "edit" ? "edit"
      : step.category === "run" ? "run"
      : step.category === "web" ? "web"
      : "work"
    counts.set(cat, (counts.get(cat) ?? 0) + 1)
  }
  const ranked = [...counts.entries()].sort((a, b) => b[1] - a[1])
  const [first, second] = ranked
  const useSecond = second && second[1] * 3 >= first[1]

  const hint = dominantPathHint(toolSteps)
  const main = (cat: LabelCategory): string => {
    switch (cat) {
      case "explore": return hint ? `Explored the ${hint} code` : "Explored the code"
      case "edit": return hint ? `Edited ${hint}` : "Edited files"
      case "run": return looksLikeTesting(toolSteps) ? "Ran tests" : "Ran commands"
      case "web": return "Browsed the web"
      default: return "Used tools"
    }
  }
  const tail = (cat: LabelCategory): string => {
    switch (cat) {
      case "explore": return "explored"
      case "edit": return "edited"
      case "run": return looksLikeTesting(toolSteps) ? "tested" : "ran commands"
      case "web": return "browsed the web"
      default: return "used tools"
    }
  }

  if (!useSecond) return main(first[0])
  if (first[0] === "edit" && second[0] === "run") return looksLikeTesting(toolSteps) ? "Edited and tested" : "Edited and ran commands"
  return `${main(first[0])} and ${tail(second[0])}`
}

export interface TimelineStats {
  turns: number
  toolCalls: number
  failures: number
}

export function timelineStats(turns: Turn[]): TimelineStats {
  let toolCalls = 0
  let failures = 0
  for (const turn of turns) {
    toolCalls += turn.toolCallCount
    failures += turn.failedCount
  }
  return { turns: turns.length, toolCalls, failures }
}

/** Latest live step of the transcript tail (running tool or model wait), if any. */
export function latestRunningStep(turns: Turn[]): Step | null {
  const last = turns[turns.length - 1]
  if (!last) return null
  for (let i = last.steps.length - 1; i >= 0; i -= 1) {
    if (last.steps[i].status === "running") return last.steps[i]
  }
  return null
}

export function formatDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return ""
  if (ms < 950) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 10) return `${s.toFixed(1)}s`
  if (s < 60) return `${Math.round(s)}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s % 60)
  if (m < 60) return rs > 0 ? `${m}m ${rs}s` : `${m}m`
  const h = Math.floor(m / 60)
  const rm = m % 60
  return rm > 0 ? `${h}h ${rm}m` : `${h}h`
}

export function formatAge(timestampMs: number, nowMs: number): string {
  const seconds = Math.max(0, Math.floor((nowMs - timestampMs) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  const remaining = seconds % 60
  if (minutes < 60) return remaining > 0 ? `${minutes}m ${remaining}s ago` : `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ago`
}

// ─── Board-card compaction (moved from conversation-view.tsx) ────────────────

export type ConversationItem =
  | { type: "message"; message: Message }
  | { type: "activity-summary"; id: string; messages: Message[]; summary?: ActivitySummaryMeta }

export function compactActivityRuns(messages: Message[]): ConversationItem[] {
  const items: ConversationItem[] = []
  let run: Message[] = []

  const flush = () => {
    const visible = run.filter((message) => !isHiddenActivity(message))
    run = []
    if (visible.length === 0) return
    items.push({
      type: "activity-summary",
      id: `activity-summary-${visible[0].id}-${visible[visible.length - 1].id}`,
      messages: visible,
    })
  }

  for (const message of messages) {
    if (message.role === "activity_summary") {
      flush()
      items.push({ type: "activity-summary", id: message.id, messages: [], summary: message.activitySummary })
      continue
    }
    if (message.role === "activity") {
      run.push(message)
      continue
    }
    flush()
    items.push({ type: "message", message })
  }
  flush()

  return items
}

/** Trailing run of live activity rows (used by board cards to surface the current step). */
export function trailingActivityRun(messages: Message[]): Message[] {
  const run: Message[] = []
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i]
    if (message.role !== "activity") break
    if (!isHiddenActivity(message)) run.unshift(message)
  }
  return run
}
