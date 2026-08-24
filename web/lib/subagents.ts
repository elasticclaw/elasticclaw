// Pure subagent derivation: extracting Task tool calls from timeline turns and
// calculating their live status. No React in here — the web app has no test
// runner, so this file is the reviewable surface for subagent behavior.

import { demoteStaleRunning, pairActivitySteps, type Step, type Turn } from "./turns"
import type { Message } from "./types"

export type { Step, ToolCategory, Turn } from "./turns"
export { formatAge, formatDurationMs } from "./turns"

export const SUBAGENT_STALE_MS = 30_000

export type SubagentStatus = "running" | "quiet" | "done" | "failed"

export interface Subagent {
  id: string
  name: string
  task: string
  type?: string
  model?: string
  status: SubagentStatus
  startedAt: Date
  endedAt?: Date
  durationMs?: number
  lastOutputAtMs: number
  result?: string
  error?: string
  turnIndex: number
  step: Step
}

function firstActivityValue(step: Step, field: "subagent_name" | "subagent_type" | "subagent_model" | "subagent_prompt"): string | undefined {
  for (const message of step.messages) {
    const value = message.activity?.[field]
    if (value?.trim()) return value
  }
  return undefined
}

function lastOutputAtMs(step: Step): number {
  let latest = step.startedAt.getTime()
  for (const message of step.messages) latest = Math.max(latest, message.timestamp.getTime())
  return latest
}

/**
 * Liveness is read from `endedAt`, not from `step.status`.
 *
 * demoteStaleRunning (turns.ts) keeps at most *one* trailing step running,
 * because the timeline was built for sequential tool calls. Subagents are the
 * opposite case: four parallel Task calls are all genuinely open, and reading
 * step.status would report three of them as "done" while they are still
 * working — the exact lie this surface exists to prevent.
 *
 * `endedAt` is only set by resolveStep, i.e. only when a terminal event for
 * that call actually arrived, so it survives the demotion untouched. A Task
 * call that never reported a terminal event therefore ages into "quiet"
 * rather than being declared finished on its behalf.
 */
function statusForStep(step: Step, outputAtMs: number, nowMs: number): SubagentStatus {
  if (step.status === "failed") return "failed"
  if (step.endedAt !== undefined) return "done"
  return nowMs - outputAtMs > SUBAGENT_STALE_MS ? "quiet" : "running"
}

function statusGroup(status: SubagentStatus): number {
  if (status === "running") return 0
  if (status === "quiet") return 1
  return 2
}

function subagentFromStep(step: Step, turnIndex: number, nowMs: number): Subagent {
  const outputAtMs = lastOutputAtMs(step)
  return {
    id: step.id,
    name: firstActivityValue(step, "subagent_name") || step.detail || "subagent",
    task: firstActivityValue(step, "subagent_prompt") || "",
    type: firstActivityValue(step, "subagent_type"),
    model: firstActivityValue(step, "subagent_model"),
    status: statusForStep(step, outputAtMs, nowMs),
    startedAt: step.startedAt,
    endedAt: step.endedAt,
    durationMs: step.durationMs,
    lastOutputAtMs: outputAtMs,
    result: step.result,
    error: step.error,
    turnIndex,
    step,
  }
}

function isSubagentStep(step: Step): boolean {
  return step.kind === "tool" && step.category === "task"
}

export function collectSubagents(turns: Turn[], nowMs: number): Subagent[] {
  const subs: Subagent[] = []

  for (const turn of turns) {
    for (const step of turn.steps) {
      if (!isSubagentStep(step)) continue
      subs.push(subagentFromStep(step, turn.index, nowMs))
    }
  }

  return subs
    .map((sub, index) => ({ sub, index }))
    .sort((a, b) => {
      const groupDifference = statusGroup(a.sub.status) - statusGroup(b.sub.status)
      if (groupDifference !== 0) return groupDifference
      const timeDifference = b.sub.lastOutputAtMs - a.sub.lastOutputAtMs
      return timeDifference !== 0 ? timeDifference : a.index - b.index
    })
    .map(({ sub }) => sub)
}

export function subagentCounts(subs: Subagent[]): { running: number; quiet: number; done: number; failed: number } {
  const counts = { running: 0, quiet: 0, done: 0, failed: 0 }
  for (const sub of subs) counts[sub.status] += 1
  return counts
}

export function subagentSummaryLine(subs: Subagent[]): string | null {
  if (subs.length === 0) return null
  const { running, quiet } = subagentCounts(subs)
  const noun = subs.length === 1 ? "subagent" : "subagents"
  const active = running + quiet
  return active > 0 ? `${active} of ${subs.length} ${noun}` : `${subs.length} ${noun} done`
}

// ─── Board/sidebar summary over a partial message window ─────────────────────

/**
 * Subagents of the current turn, derived from a raw message window — or null
 * when that window cannot answer the question honestly.
 *
 * The board keeps a *recent-activity window*, not the full history: the
 * timeline endpoint returns `activity_summary` placeholders, and the prefetch
 * expands only the trailing ones within a row budget, so a partially expanded
 * summary survives with `count > 0`. If such a remainder sits inside the
 * current turn, some Task calls of that turn are simply not loaded and any
 * count derived here would be an undercount presented as fact.
 *
 * So this returns null — meaning "say nothing" — whenever either is true:
 *   - the window contains no user message, so it does not reach the start of
 *     the current turn and the turn may have begun before the window;
 *   - an unexpanded summary (count > 0) sits at or after that user message.
 *
 * A wrong number is worse than no number.
 */
export function currentTurnSubagents(messages: Message[], nowMs: number): Subagent[] | null {
  let turnStart = -1
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i].role === "user") {
      turnStart = i
      break
    }
  }
  if (turnStart === -1) return null

  const window = messages.slice(turnStart)
  for (const message of window) {
    if (message.role === "activity_summary" && (message.activitySummary?.count ?? 0) > 0) return null
  }

  // keepTrailingRunning: only the newest dangling step may still be live, the
  // same rule the sidebar activity line uses.
  const steps = demoteStaleRunning(pairActivitySteps(window), true)
  return steps
    .filter(isSubagentStep)
    .map((step) => subagentFromStep(step, 0, nowMs))
}

/** Sidebar line for one claw, or null when the window cannot be trusted. */
export function currentTurnSubagentLine(messages: Message[], nowMs: number): string | null {
  const subs = currentTurnSubagents(messages, nowMs)
  if (!subs || subs.length === 0) return null
  return subagentSummaryLine(subs)
}
