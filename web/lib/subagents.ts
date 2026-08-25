// Pure subagent derivation: extracting Task tool calls from timeline turns and
// calculating their live status. No React in here — the web app has no test
// runner, so this file is the reviewable surface for subagent behavior.

import { demoteStaleRunning, isTerminalActivity, pairActivitySteps, type Step, type Turn } from "./turns"
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
  /**
   * Ids of every timeline step this subagent was folded out of — the start
   * step plus any orphan terminal fragment. The chat view resolves a clicked
   * row through these, so a fragment row opens the same drill-down as the
   * start row instead of resolving to nothing.
   */
  stepIds: string[]
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

function subagentFromStep(step: Step, turnIndex: number, nowMs: number, stepIds?: string[]): Subagent {
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
    stepIds: stepIds ?? [step.id],
  }
}

/**
 * Tools that actually spawn a subagent — the same set the bridge uses to decide
 * whether to attach `subagent_*` fields (isSubagentTool, cmd/claw-bridge/main.go).
 *
 * `step.category === "task"` is far too wide: toolCategory maps
 * /task|agent|skill|workflow|dispatch/ to "task", so Skill, TaskStop and
 * spawn_task calls would be rendered as nameless, promptless subagents in the
 * rail, counted in every header, and turned into drill-down rows whose result
 * no longer expands inline.
 */
const SUBAGENT_TOOLS = new Set(["task", "agent", "subagent"])

export function isSubagentStep(step: Step): boolean {
  if (step.kind !== "tool" || step.category !== "task") return false
  for (const message of step.messages) {
    const tool = message.activity?.tool?.trim().toLowerCase()
    if (tool && SUBAGENT_TOOLS.has(tool)) return true
  }
  return false
}

/**
 * Identity of the underlying tool call.
 *
 * A step run is flushed at every user/claw/hub message (buildTurn.flushRun),
 * so a Task whose terminal event arrives after an interleaved message is
 * paired as two steps: a start with no `endedAt` and an orphan terminal. Both
 * describe one call, and `call_id` is what ties them together.
 *
 * The key is only ever compared *within one turn*: when the gateway supplies
 * no call_id the bridge synthesizes one from a per-turn counter over a
 * signature that is identical for every Task call, so the first Task of every
 * turn gets the same id. Comparing those across turns folded unrelated
 * subagents into one card — and a finished older call then reported the still
 * running newer one as done.
 */
function subagentCallKey(step: Step): string {
  for (const message of step.messages) {
    const callId = message.activity?.call_id?.trim()
    if (callId) return `call:${callId}`
  }
  return `step:${step.id}`
}

/**
 * A terminal fragment whose start event is not in this window — the
 * "terminal without a visible start" branch of pairActivitySteps.
 */
function isOrphanTerminalStep(step: Step): boolean {
  return step.endedAt !== undefined && step.messages.length === 1 && isTerminalActivity(step.messages[0])
}

/** Fold a later fragment of the same call into the earlier one. */
function mergeSubagentSteps(base: Step, next: Step): Step {
  const resolved = next.endedAt !== undefined
  return {
    ...base,
    status: resolved || next.status === "failed" ? next.status : base.status,
    tone: resolved ? next.tone : base.tone,
    detail: base.detail || next.detail,
    statusText: next.statusText || base.statusText,
    endedAt: next.endedAt ?? base.endedAt,
    durationMs: next.endedAt
      ? next.durationMs ?? Math.max(0, next.endedAt.getTime() - base.startedAt.getTime())
      : base.durationMs,
    exitCode: next.exitCode ?? base.exitCode,
    result: next.result ?? base.result,
    error: next.error ?? base.error,
    messages: [...base.messages, ...next.messages],
  }
}

/**
 * Newest output timestamp among subagent calls with no terminal event, or 0.
 *
 * The panel uses it to decide whether its clock still has to tick: a claw that
 * dies mid-Task stops producing turns, so without this its open subagents
 * would stay frozen on "running" forever. Once every open call is past
 * SUBAGENT_STALE_MS nothing derived here can change again.
 */
export function latestOpenSubagentOutputMs(turns: Turn[]): number {
  let latest = 0
  for (const turn of turns) {
    for (const step of turn.steps) {
      if (!isSubagentStep(step) || step.endedAt !== undefined || step.status === "failed") continue
      latest = Math.max(latest, lastOutputAtMs(step))
    }
  }
  return latest
}

export function collectSubagents(turns: Turn[], nowMs: number): Subagent[] {
  const byCall = new Map<string, { step: Step; turnIndex: number; stepIds: string[] }>()
  const order: string[] = []
  // call key → map key of the newest still-unresolved subagent for that call.
  // Only an orphan terminal is allowed to reach back through it.
  const openByCall = new Map<string, string>()

  for (const turn of turns) {
    for (const step of turn.steps) {
      if (!isSubagentStep(step)) continue
      const callKey = subagentCallKey(step)
      // Turn-scoped: see subagentCallKey — synthesized call ids repeat across turns.
      let key = `${turn.index}\u0000${callKey}`
      const existing = byCall.get(key)
      if (existing) {
        // A call that already reported a terminal event is finished; a later
        // step carrying the same id is a different call reusing it, not a
        // fragment of this one.
        if (existing.step.endedAt === undefined) {
          existing.step = mergeSubagentSteps(existing.step, step)
          existing.stepIds.push(step.id)
          if (existing.step.endedAt !== undefined && openByCall.get(callKey) === key) {
            openByCall.delete(callKey)
          }
          continue
        }
        key = `${key}\u0000${step.id}`
      } else if (isOrphanTerminalStep(step)) {
        // A Task still running when the user sends the next message reports its
        // terminal inside the *following* turn, where pairActivitySteps cannot
        // see the start. Left unmerged it renders twice: a card stuck live
        // forever plus a nameless "done" one. Only orphan terminals may cross
        // the turn boundary — a *start* carrying a reused synthesized id is
        // genuinely a new call, which is why the key stays turn-scoped.
        //
        // The reach-back spans any number of turns, not just the previous one:
        // a Task the user talks over keeps running while further turns come and
        // go, and its terminal lands in whichever turn happens to be open when
        // it finishes. Bounding it to the immediately preceding turn left those
        // calls split in two — a card stuck "quiet" forever plus a nameless
        // "done" one, reported by the rail header as two subagents.
        //
        // openByCall holds only the *newest* unresolved call for the key, so a
        // later start reclaims it and a repeated synthesized id cannot drag a
        // result onto an older card. The residual case is a Task that never
        // reported a terminal and whose successor's start row is missing from
        // the window; merging there is preferable to rendering one call twice.
        const openKey = openByCall.get(callKey)
        const open = openKey ? byCall.get(openKey) : undefined
        if (open && open.step.endedAt === undefined) {
          open.step = mergeSubagentSteps(open.step, step)
          open.stepIds.push(step.id)
          openByCall.delete(callKey)
          continue
        }
      }
      byCall.set(key, { step, turnIndex: turn.index, stepIds: [step.id] })
      order.push(key)
      if (step.endedAt === undefined) openByCall.set(callKey, key)
      else if (openByCall.get(callKey) === key) openByCall.delete(callKey)
    }
  }

  return order
    .map((key, index) => {
      const entry = byCall.get(key)!
      return { sub: subagentFromStep(entry.step, entry.turnIndex, nowMs, entry.stepIds), index }
    })
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
  const { running, quiet, failed } = subagentCounts(subs)
  const noun = subs.length === 1 ? "subagent" : "subagents"
  const active = running + quiet
  // A failure is the one state a user scanning the board most needs to see, so
  // it is never folded into "done".
  if (active === 0) {
    if (failed === subs.length) return `${failed} ${noun} failed`
    if (failed > 0) return `${subs.length - failed} of ${subs.length} ${noun} done · ${failed} failed`
    return `${subs.length} ${noun} done`
  }
  // "1 of 1 subagent" says nothing; when everything is still open, say so.
  const line = active === subs.length ? `${active} ${noun} running` : `${active} of ${subs.length} ${noun}`
  return failed > 0 ? `${line} · ${failed} failed` : line
}

// ─── Board/sidebar summary over a partial message window ─────────────────────

/**
 * Subagents of the current turn, derived from a raw message window — or null
 * when that window cannot answer the question honestly.
 *
 * The board keeps a *recent-activity window*, not the full history: the
 * timeline endpoint returns `activity_summary` placeholders, and the prefetch
 * expands only the trailing ones within a row budget, so a partially expanded
 * summary survives with `count > 0`. The live cache truncates too — a long
 * turn past MAX_LIVE_ACTIVITIES_PER_CLAW has its oldest activity rows pruned —
 * which is why pruneOldestLiveActivities leaves an `activity_summary` marker
 * for what it removed instead of deleting silently. If such a remainder sits
 * inside the current turn, some Task calls of that turn are simply not loaded
 * and any count derived here would be an undercount presented as fact.
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
    // A Task still in flight when the user sent this turn's message reports its
    // terminal inside this window while its start sits before it, so the pairing
    // yields a terminal-without-start fragment. It belongs to the previous turn:
    // counting it here would report "1 of 2 subagents" for a turn that launched
    // one. Unlike collectSubagents there is nothing to merge it into, so it is
    // dropped.
    .filter((step) => isSubagentStep(step) && !isOrphanTerminalStep(step))
    .map((step) => subagentFromStep(step, 0, nowMs))
}

/** Sidebar line for one claw, or null when the window cannot be trusted. */
export function currentTurnSubagentLine(messages: Message[], nowMs: number): string | null {
  const subs = currentTurnSubagents(messages, nowMs)
  if (!subs || subs.length === 0) return null
  return subagentSummaryLine(subs)
}
