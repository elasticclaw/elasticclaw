import type { AgentActivity, Message } from "@/lib/types"

export type TranscriptClawMeta = {
  id: string
  name: string
  status?: string
  template?: string
  githubIssueId?: string
  githubIssueUrl?: string
}

/** Human-readable role for debug transcripts. */
export function transcriptRoleLabel(message: Message): string {
  switch (message.role) {
    case "user":
      return "user"
    case "claw":
      return "agent"
    case "hub":
      return "hub (injected)"
    case "system":
      return "system"
    case "activity": {
      const a = message.activity
      if (!a) return "activity"
      const parts = [a.kind]
      if (a.tool) parts.push(a.tool)
      if (a.phase) parts.push(a.phase)
      return `activity (${parts.filter(Boolean).join(": ")})`
    }
    case "activity_summary": {
      const n = message.activitySummary?.count
      return typeof n === "number" ? `activity_summary (count=${n})` : "activity_summary"
    }
    default:
      return String(message.role)
  }
}

function toISO(ts: Date | string): string {
  const d = ts instanceof Date ? ts : new Date(ts)
  if (Number.isNaN(d.getTime())) return String(ts)
  return d.toISOString()
}

function trimBody(text: string): string {
  return text.replace(/\s+$/u, "")
}

/** Serialize structured AgentActivity fields for debug paste. */
export function formatActivityTranscriptBody(activity: AgentActivity): string {
  const lines: string[] = []
  const push = (key: string, value: string | undefined) => {
    const v = value?.trim()
    if (v) lines.push(`${key}: ${v}`)
  }
  push("kind", activity.kind)
  push("tool", activity.tool)
  push("phase", activity.phase)
  push("stream", activity.stream)
  push("command", activity.command)
  push("path", activity.path)
  push("url", activity.url)
  push("detail", activity.detail)
  push("message", activity.message)
  push("error", activity.error)
  return lines.join("\n")
}

/**
 * Body text for one transcript row. Prefer structured activity / summary
 * fields so copied logs match what the UI shows (commands, paths, errors).
 */
export function formatTranscriptMessageBody(message: Message): string {
  if (message.role === "activity" && message.activity) {
    const structured = formatActivityTranscriptBody(message.activity)
    const content = trimBody(message.content ?? "")
    // Prefer structured fields; append content only when it adds new info.
    if (structured) {
      if (content && !structured.includes(content)) {
        return `${structured}\ncontent: ${content}`
      }
      return structured
    }
    if (content) return content
    return "(empty activity)"
  }

  if (message.role === "activity_summary") {
    const summary = message.activitySummary
    const lines: string[] = []
    if (summary) {
      lines.push(`count: ${summary.count}`)
      if (summary.from) lines.push(`from: ${summary.from}`)
      if (summary.to) lines.push(`to: ${summary.to}`)
    }
    const content = trimBody(message.content ?? "")
    if (content) lines.push(content)
    if (lines.length === 0) return "(empty activity summary)"
    return lines.join("\n")
  }

  const body = trimBody(message.content ?? "")
  return body || "(empty)"
}

/**
 * Format a claw conversation for clipboard paste into debugging tools.
 * Includes role labels (user / agent / hub injected / system / activity)
 * and ISO timestamps per message.
 */
export function formatChatTranscript(opts: {
  claw: TranscriptClawMeta
  messages: Message[]
  streamingText?: string
}): string {
  const { claw, messages, streamingText } = opts
  const sorted = [...messages].sort((a, b) => {
    const ta = a.timestamp instanceof Date ? a.timestamp.getTime() : new Date(a.timestamp).getTime()
    const tb = b.timestamp instanceof Date ? b.timestamp.getTime() : new Date(b.timestamp).getTime()
    return ta - tb
  })

  const lines: string[] = [
    "# ElasticClaw chat transcript",
    `claw: ${claw.name}`,
    `id: ${claw.id}`,
  ]
  if (claw.template) lines.push(`template: ${claw.template}`)
  if (claw.status) lines.push(`status: ${claw.status}`)
  if (claw.githubIssueId) lines.push(`issue: ${claw.githubIssueId}`)
  if (claw.githubIssueUrl) lines.push(`issue_url: ${claw.githubIssueUrl}`)
  lines.push(`exported: ${new Date().toISOString()}`)
  lines.push(
    `message_count: ${sorted.length}${streamingText?.trim() ? " (+ streaming tail)" : ""}`
  )
  lines.push("")
  lines.push("---")
  lines.push("")

  for (const message of sorted) {
    const label = transcriptRoleLabel(message)
    lines.push(`[${toISO(message.timestamp)}] ${label}`)
    lines.push(formatTranscriptMessageBody(message))
    lines.push("")
  }

  const stream = streamingText?.replace(/\s+$/u, "")
  if (stream) {
    lines.push(`[${new Date().toISOString()}] agent (streaming)`)
    lines.push(stream)
    lines.push("")
  }

  return lines.join("\n")
}

/** Copy text to the clipboard; returns true on success. */
export async function copyTextToClipboard(text: string): Promise<boolean> {
  try {
    if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // fall through to execCommand
  }
  try {
    const el = document.createElement("textarea")
    el.value = text
    el.setAttribute("readonly", "")
    el.style.position = "fixed"
    el.style.left = "-9999px"
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand("copy")
    document.body.removeChild(el)
    return ok
  } catch {
    return false
  }
}
