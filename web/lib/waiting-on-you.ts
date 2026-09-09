import type { Message } from "@/lib/types"

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

export function isWaitingOnYou(messages: Message[]): boolean {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i]
    if (message.role === "system" || message.role === "activity" || message.role === "activity_summary") continue
    return message.role === "claw" && Boolean(extractQuestion(message.content))
  }
  return false
}
