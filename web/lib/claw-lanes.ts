import type { Claw, Message } from "@/lib/types"
import type { NowStripState } from "@/components/agent-timeline/now-strip"
import { windowMessagesByDurableCount } from "@/lib/messages"
import {
  demoteStaleRunning,
  pairActivitySteps,
  trailingActivityRun,
  type Step,
} from "@/lib/turns"

/** Board cards read a bounded tail of the transcript, not the whole history. */
export const BOARD_CARD_DURABLE_MESSAGE_WINDOW = 50

function firstMeaningfulLine(text: string): string {
  for (const line of text.split("\n")) {
    const trimmed = line.replace(/^#+\s*/, "").trim()
    if (trimmed) return trimmed
  }
  return text.trim()
}

/**
 * The last question sentence of a message, if it plausibly asks the user
 * something ("posso forçar push?"). The "?" must end the message or a
 * sentence; the question starts after the previous sentence/line break.
 */
export function extractQuestion(text: string): string | null {
  const trimmed = text.trim()
  const qIdx = trimmed.lastIndexOf("?")
  if (qIdx === -1) return null
  if (qIdx !== trimmed.length - 1 && !/\s/.test(trimmed[qIdx + 1])) return null
  const before = trimmed.slice(0, qIdx)
  const boundary = before.match(/[.!?\n][^.!?\n]*$/)
  const start = boundary?.index !== undefined ? boundary.index + 1 : 0
  const question = trimmed.slice(start, qIdx + 1).trim()
  return question || null
}

function lastActivityError(messages: Message[]): string | null {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i]
    if (m.role === "activity" && m.activity?.error) return m.activity.error
  }
  return null
}

export interface BoardCardNow {
  state: NowStripState
  text?: string
  at?: number | null
}

/**
 * The card's status line: is the agent working, waiting on the user, finished,
 * broken, or gone — and the one-liner that proves it. Working defers to the
 * NowStrip's live content (tool + elapsed + last-output age).
 */
export function boardCardNow(
  claw: Claw,
  messages: Message[],
  latestStep: Step | null,
  isStreaming: boolean
): BoardCardNow | null {
  // BootstrapProgress owns the provisioning story.
  if (claw.status === "provisioning") return null

  let lastAt: number | null = null
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i].role === "system") continue
    lastAt = messages[i].timestamp.getTime()
    break
  }

  if (claw.status === "error") {
    return {
      state: "error",
      text: claw.reason || lastActivityError(messages) || "Agent errored",
      at: lastAt,
    }
  }
  if (claw.status === "offline") {
    const seen = claw.last_seen ? new Date(claw.last_seen).getTime() : NaN
    return { state: "offline", at: Number.isFinite(seen) ? seen : lastAt }
  }
  if (isStreaming || latestStep?.status === "running") return { state: "working" }

  // Nothing live: surface what the agent ended with. Transcript tail decides —
  // trailing tool activity beats older prose, a question flags "needs you".
  if (latestStep) {
    return {
      state: "done",
      text: latestStep.detail ? `${latestStep.title} · ${latestStep.detail}` : latestStep.title,
      at: (latestStep.endedAt ?? latestStep.startedAt).getTime(),
    }
  }
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i]
    if (m.role === "system" || m.role === "activity" || m.role === "activity_summary") continue
    const at = m.timestamp.getTime()
    if (m.role === "claw") {
      const text = m.content.trim()
      const question = extractQuestion(text)
      if (question) return { state: "waiting", text: question, at }
      return { state: "done", text: firstMeaningfulLine(text), at }
    }
    if (m.role === "hub") return { state: "done", text: m.content, at }
    if (m.role === "user") return { state: "done", text: "Waiting to start", at }
  }
  return { state: "done", text: "Idle", at: lastAt }
}

/** An offline/errored claw cannot still be running its dangling last step. */
export function allowsTrailingRunning(claw: Claw): boolean {
  return claw.status !== "offline" && claw.status !== "error"
}

/** Latest step of the trailing activity run — the card's live status source. */
export function trailingLatestStep(claw: Claw, visibleMessages: Message[]): Step | null {
  const steps = demoteStaleRunning(
    pairActivitySteps(trailingActivityRun(visibleMessages)),
    allowsTrailingRunning(claw)
  )
  return steps.length > 0 ? steps[steps.length - 1] : null
}

/**
 * The three status lanes the board and the sidebar are grouped by.
 * "attention" is anything blocked on the user, "working" is live, "idle"
 * is everything that finished or went away.
 */
export type ClawLane = "attention" | "working" | "idle"

export const CLAW_LANE_ORDER: ClawLane[] = ["attention", "working", "idle"]

export const CLAW_LANE_META: Record<ClawLane, { title: string; note: string }> = {
  attention: { title: "Needs you", note: "Blocked until you answer or restart" },
  working: { title: "Working", note: "Live — no action needed" },
  idle: { title: "Idle & offline", note: "Finished or disconnected" },
}

/**
 * Lane membership, derived from the same signals the board card already shows
 * in its state strip — no extra data is fetched. Errors (which include a
 * failed provisioning) and a trailing unanswered question mean the agent is
 * stuck on the user; a live stream or a running tool step means it is working;
 * everything else (finished turn, offline) is idle.
 */
export function clawLane(claw: Claw, messages: Message[], isStreaming: boolean): ClawLane {
  if (claw.status === "error") return "attention"
  // Still booting: nothing to answer yet, and it is on its way to running.
  if (claw.status === "provisioning") return "working"
  if (claw.status === "offline") return "idle"
  if (isStreaming) return "working"

  const visible = windowMessagesByDurableCount(messages, BOARD_CARD_DURABLE_MESSAGE_WINDOW)
  const now = boardCardNow(claw, visible, trailingLatestStep(claw, visible), isStreaming)
  if (!now) return "working"
  switch (now.state) {
    case "error":
    case "waiting":
      return "attention"
    case "working":
      return "working"
    default:
      return "idle"
  }
}

/** Lane per claw id, ready to hand to the board and the sidebar. */
export function clawLanes(
  claws: Claw[],
  messages: Record<string, Message[]>,
  isStreaming: (claw: Claw) => boolean
): Record<string, ClawLane> {
  const lanes: Record<string, ClawLane> = {}
  for (const claw of claws) {
    lanes[claw.id] = clawLane(claw, messages[claw.id] ?? [], isStreaming(claw))
  }
  return lanes
}
