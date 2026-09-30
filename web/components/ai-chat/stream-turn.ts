import type { AIChatMessage } from "@/lib/api"

// Progress of one streamed turn, updated in place as SSE events arrive.
export type StreamTurn = {
  threadId: string
  text: string
  retry: boolean
  started: boolean
  assistantId: string
  error: string
}

export type StreamView = {
  setMessages: (update: (messages: AIChatMessage[]) => AIChatMessage[]) => void
  setDraft: (update: (draft: string) => string) => void
  setReading: (reading: string) => void
}

export function applyStreamEvent(turn: StreamTurn, event: string, payload: Record<string, unknown>, view: StreamView) {
  switch (event) {
    case "message_started": {
      if (typeof payload.messageId !== "string") return
      turn.started = true
      turn.assistantId = payload.messageId
      // Clear the composer only if it still holds what was just sent; a retry
      // sends no text, and the user may have started typing something else.
      if (!turn.retry) view.setDraft((draft) => (draft.trim() === turn.text.trim() ? "" : draft))
      const base = { threadId: turn.threadId, seq: 0, model: "", inputTokens: 0, outputTokens: 0, createdAt: Date.now() }
      view.setMessages((previous) => [...previous,
        ...(!turn.retry ? [{ ...base, id: `user-${turn.assistantId}`, role: "user" as const, content: turn.text, status: "completed" }] : []),
        { ...base, id: turn.assistantId, role: "assistant", content: "", status: "streaming", model: String(payload.model ?? "") },
      ])
      break
    }
    case "token":
      if (typeof payload.text === "string") updateAssistant(turn, view, (message) => ({ ...message, content: message.content + payload.text }))
      break
    case "tool_started": view.setReading(String(payload.provider || payload.tool || "sources")); break
    case "tool_finished": view.setReading(""); break
    case "error": turn.error = typeof payload.message === "string" ? payload.message : "Unable to complete this turn."; break
    case "done": updateAssistant(turn, view, (message) => ({ ...message, status: String(payload.status) })); break
    // Reserved events and unknown block types are intentionally ignored.
    default: break
  }
}

function updateAssistant(turn: StreamTurn, view: StreamView, update: (message: AIChatMessage) => AIChatMessage) {
  view.setMessages((previous) => previous.map((message) => message.id === turn.assistantId ? update(message) : message))
}
