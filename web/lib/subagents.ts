// Pure subagent derivation: extracting Task tool calls from timeline turns and
// calculating their live status. No React in here — the web app has no test
// runner, so this file is the reviewable surface for subagent behavior.

import type { Step, Turn } from "./turns"

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

function statusForStep(step: Step, outputAtMs: number, nowMs: number): SubagentStatus {
  if (step.status === "failed") return "failed"
  if (step.status === "ok") return "done"
  return nowMs - outputAtMs > SUBAGENT_STALE_MS ? "quiet" : "running"
}

function statusGroup(status: SubagentStatus): number {
  if (status === "running") return 0
  if (status === "quiet") return 1
  return 2
}

export function collectSubagents(turns: Turn[], nowMs: number): Subagent[] {
  const subs: Subagent[] = []

  for (const turn of turns) {
    for (const step of turn.steps) {
      if (step.kind !== "tool" || step.category !== "task") continue
      const outputAtMs = lastOutputAtMs(step)
      const name = firstActivityValue(step, "subagent_name") || step.detail || "subagent"
      subs.push({
        id: step.id,
        name,
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
        turnIndex: turn.index,
        step,
      })
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
