import type { AIChatMessage, AIChatAnswers, AIChatAnswersSummary, AIChatInterview, AIChatBlock, AIChatArtifactRequest, AIChatArtifactCard } from "@/lib/api"

// Progress of one streamed turn, updated in place as SSE events arrive.
export type StreamTurn = {
  action?: AIChatArtifactRequest
  answers?: AIChatAnswers
  answersSummary?: AIChatAnswersSummary
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
      if (!turn.retry && !turn.answers && !turn.action) view.setDraft((draft) => (draft.trim() === turn.text.trim() ? "" : draft))
      const base = { threadId: turn.threadId, seq: 0, model: "", inputTokens: 0, outputTokens: 0, createdAt: Date.now() }
      view.setMessages((previous) => [...previous.map((message) =>
        turn.answers && message.role === "assistant" && message.interview?.questionSetId === turn.answers.questionSetId && "steps" in message.interview
          ? { ...message, interview: { ...message.interview, answered: true } } : message),
        ...(!turn.retry && !turn.action ? [{ ...base, id: `user-${turn.assistantId}`, role: "user" as const, content: turn.text, interview: turn.answersSummary, status: "completed" }] : []),
        { ...base, id: turn.assistantId, role: "assistant", content: "", status: "streaming", model: String(payload.model ?? "") },
      ])
      break
    }
    case "token":
      if (typeof payload.text === "string") updateAssistant(turn, view, (message) => ({ ...message, content: message.content + payload.text }))
      break
    case "block":
      if (["stat_tiles", "callout", "table", "actions"].includes(String(payload.type)) && typeof payload.id === "string" && typeof payload.at === "number" && Number.isInteger(payload.at) && payload.at >= 0) {
        updateAssistant(turn, view, (message) => ({ ...message, blocks: [...(message.blocks ?? []), payload as AIChatBlock] }))
      }
      break
    case "action":
      if (payload.kind === "artifact" && ["working", "created", "failed"].includes(String(payload.state)) && payload.card && typeof payload.card === "object" && typeof (payload.card as AIChatArtifactCard).title === "string") {
        updateAssistant(turn, view, (message) => {
          const blocks = message.blocks ?? []
          const previous = blocks.find((block) => block.type === "action" && block.kind === "artifact")
          const block: AIChatBlock = { id: previous?.id ?? `${message.id}-artifact`, at: previous?.at ?? Array.from(message.content).length, type: "action", kind: "artifact", state: payload.state as "working" | "created" | "failed", card: payload.card as AIChatArtifactCard }
          return { ...message, blocks: previous ? blocks.map((item) => item === previous ? block : item) : [...blocks, block] }
        })
      }
      break
    case "interview":
      if (typeof payload.questionSetId === "string" && typeof payload.title === "string" && Array.isArray(payload.steps)) {
        updateAssistant(turn, view, (message) => ({ ...message, interview: { questionSetId: payload.questionSetId, title: payload.title, steps: payload.steps, answered: false } as AIChatInterview }))
      }
      break
    case "tool_started": view.setReading(String(payload.provider || payload.tool || "sources")); break
    case "tool_finished": view.setReading(""); break
    case "error": turn.error = typeof payload.message === "string" ? payload.message : "Unable to complete this turn."; break
    case "done": updateAssistant(turn, view, (message) => ({ ...message, status: String(payload.status) })); break
    // Future events are intentionally ignored.
    default: break
  }
}

function updateAssistant(turn: StreamTurn, view: StreamView, update: (message: AIChatMessage) => AIChatMessage) {
  view.setMessages((previous) => previous.map((message) => message.id === turn.assistantId ? update(message) : message))
}

export function summarizeInterview(interview: AIChatInterview, answers: AIChatAnswers): AIChatAnswersSummary {
  return {
    questionSetId: interview.questionSetId, title: interview.title,
    skipped: answers.steps.filter((step) => step.skipped).length,
    answers: interview.steps.flatMap((step, index) => {
      const submitted = answers.steps[index]
      if (!submitted || submitted.skipped) return []
      return step.questions.map((question) => {
        const answer = submitted.answers.find((item) => item.questionId === question.id)
        return { question: question.label || question.prompt, answer: [...(answer?.choices ?? []), ...(answer?.other?.trim() ? [answer.other.trim()] : [])].join(", ") }
      })
    }),
  }
}
