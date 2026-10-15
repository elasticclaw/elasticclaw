"use client"

import { useEffect, useRef } from "react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { Sparkles } from "lucide-react"
import { AnswersSummary, InlineBlock, splitContent } from "./blocks"
import { TicketActionCard } from "./ticket-card"
import { ArtifactActionCard } from "./artifact-card"
import { InterviewCard } from "./interview-card"
import type { AIChatArtifactRequest, AIChatTicketRequest, AIChatTicketTarget, AIChatAnswersSummary, AIChatMessage, AIChatStepAnswers } from "@/lib/api"

function isAnswers(message: AIChatMessage): message is AIChatMessage & { interview: AIChatAnswersSummary } {
  return message.role === "user" && !!message.interview && "answers" in message.interview
}

export function MessageList({ messages, reading, streaming = false, interviewDisabled = false, onAnswer, onArtifact, onTicket, onDismissTicket, ticket, onCancel }: { messages: AIChatMessage[]; reading: string; streaming?: boolean; interviewDisabled?: boolean; onArtifact?: (action?: AIChatArtifactRequest) => void; onTicket?: (action?: AIChatTicketRequest) => void; onDismissTicket?: (attemptId: string) => Promise<void>; ticket?: AIChatTicketTarget; onCancel?: () => void; onAnswer?: (id: string, steps: AIChatStepAnswers[]) => void }) {
  const end = useRef<HTMLDivElement>(null)
  useEffect(() => { end.current?.scrollIntoView({ block: "end" }) }, [messages, reading])
  const tickets = messages.flatMap((message) => message.blocks ?? []).filter((block) => block.type === "action" && block.kind === "ticket")
  const resolved = new Set(tickets.map((block) => block.card.confirmedUnknown).filter(Boolean))
  const dismissedAttempts = new Set<string>()
  // Replayed refusals share the attempt id, so one dismissal settles every card for it.
  for (const block of tickets) if (block.card.dismissed) {
    const attemptId = block.card.attemptId || block.id.replace(/-ticket$/, "")
    dismissedAttempts.add(attemptId)
    resolved.add(attemptId)
  }
  const latestUnknown = tickets.filter((block) => block.state === "failed" && block.card.outcomeUnknown && !resolved.has(block.card.attemptId || block.id.replace(/-ticket$/, ""))).at(-1)
  return (
    <div className="space-y-6" role="log" aria-label="Conversation">
      {messages.map((message, index) => {
        if (isAnswers(message) && messages[index + 1]?.role === "assistant") return null
        // Interview answers read as part of the reply they produced (lite-02), not as a bubble.
        if (isAnswers(message)) return <article key={message.id} className="text-sm sm:pl-7"><AnswersSummary summary={message.interview} /></article>
        const previous = messages[index - 1]
        const latest = !interviewDisabled && !streaming && index === messages.length - 1
        const ticketAction = latest && ticket ? onTicket : undefined
        return <article key={message.id} className={message.role === "user" ? "ml-auto w-fit max-w-[85%] rounded-lg border border-(--step-running)/50 bg-(--step-running)/20 px-3.5 py-2.5" : "text-sm sm:pl-7"}>
          <div className={`flex items-center gap-2 text-xs text-muted-foreground ${message.role === "user" ? "mb-1" : "mb-2 sm:-ml-7"}`}>
            {message.role === "assistant" && <span className="inline-flex size-5 items-center justify-center rounded-md bg-muted"><Sparkles className="size-3" /></span>}
            <span className="font-medium text-foreground">{message.role === "user" ? "You" : "Assistant"}</span>
            <time dateTime={new Date(message.createdAt).toISOString()}>{new Date(message.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" })}</time>
          </div>
          {message.role === "user" ? <p className="whitespace-pre-wrap break-words text-sm">{message.content}</p> : (
            <div className="space-y-3 break-words leading-relaxed [&>*:first-child]:mt-0 [&_p]:my-3 [&_p:first-child]:mt-0 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:list-decimal [&_ol]:pl-5 [&_li]:my-1.5 [&_li]:text-muted-foreground [&_li_strong]:text-foreground [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-3 [&_a:not([data-slot=button])]:text-primary [&_a:not([data-slot=button])]:underline">
              {previous && isAnswers(previous) && <AnswersSummary summary={previous.interview} />}
              {splitContent(message.content || (!message.blocks?.length && !message.interview && message.status === "streaming" ? "Thinking…" : ""), message.blocks).map((part, partIndex) => typeof part === "string" ?
                <ReactMarkdown key={partIndex} remarkPlugins={[remarkGfm]} components={{ img: () => null, a: ({ children, ...props }) => <a {...props} target="_blank" rel="noreferrer">{children}</a> }}>{part}</ReactMarkdown> : part.type === "action" && part.kind === "ticket" ? <TicketActionCard key={part.id} block={part.card.outcomeUnknown && dismissedAttempts.has(part.card.attemptId || part.id.replace(/-ticket$/, "")) ? { ...part, card: { ...part.card, dismissed: true } } : part} disabled={streaming} onDismiss={part === latestUnknown ? onDismissTicket : undefined} onRetry={part.card.outcomeUnknown ? (!interviewDisabled && part === latestUnknown && ticket ? onTicket : undefined) : ticketAction} /> : part.type === "action" ? <ArtifactActionCard key={part.id} block={part} ticket={ticket} onTicket={ticketAction} onStop={streaming ? onCancel : undefined} onRetry={latest ? onArtifact : undefined} /> : <InlineBlock key={part.id} block={part.type === "actions" && !ticket ? { ...part, actions: part.actions.filter((kind) => kind !== "ticket") } : part} onAction={latest && message.status === "completed" && (onArtifact || ticketAction) ? (kind) => { if (kind === "artifact") onArtifact?.(); else if (kind === "ticket") ticketAction?.() } : undefined} />)}
              {message.interview && "steps" in message.interview && !message.interview.answered && <InterviewCard key={message.interview.questionSetId} interview={message.interview} disabled={interviewDisabled || streaming || message.status !== "completed" || index !== messages.length - 1} onAnswer={onAnswer} />}
            </div>
          )}
          {message.status === "cancelled" && <p className="mt-2 text-xs text-muted-foreground">Response cancelled.</p>}
          {(message.status === "error" || message.status === "limit") && <p className="mt-2 text-xs text-destructive">This response did not finish.</p>}
        </article>
      })}
      {reading && <p role="status" className="text-xs text-muted-foreground">Reading {reading}…</p>}
      <div ref={end} />
    </div>
  )
}
