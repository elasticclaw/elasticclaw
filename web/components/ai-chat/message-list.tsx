"use client"

import { useEffect, useRef } from "react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { Sparkles } from "lucide-react"
import { AnswersSummary, InlineBlock, splitContent } from "./blocks"
import { ArtifactActionCard } from "./artifact-card"
import { InterviewCard } from "./interview-card"
import type { AIChatArtifactRequest, AIChatAnswersSummary, AIChatMessage, AIChatStepAnswers } from "@/lib/api"

function isAnswers(message: AIChatMessage): message is AIChatMessage & { interview: AIChatAnswersSummary } {
  return message.role === "user" && !!message.interview && "answers" in message.interview
}

export function MessageList({ messages, reading, streaming = false, interviewDisabled = false, onAnswer, onArtifact, onCancel }: { messages: AIChatMessage[]; reading: string; streaming?: boolean; interviewDisabled?: boolean; onArtifact?: (action?: AIChatArtifactRequest) => void; onCancel?: () => void; onAnswer?: (id: string, steps: AIChatStepAnswers[]) => void }) {
  const end = useRef<HTMLDivElement>(null)
  useEffect(() => { end.current?.scrollIntoView({ block: "end" }) }, [messages, reading])
  return (
    <div className="space-y-6" role="log" aria-label="Conversation">
      {messages.map((message, index) => {
        if (isAnswers(message) && messages[index + 1]?.role === "assistant") return null
        // Interview answers read as part of the reply they produced (lite-02), not as a bubble.
        if (isAnswers(message)) return <article key={message.id} className="text-sm sm:pl-7"><AnswersSummary summary={message.interview} /></article>
        const previous = messages[index - 1]
        return <article key={message.id} className={message.role === "user" ? "ml-auto w-fit max-w-[85%] rounded-lg border border-(--step-running)/50 bg-(--step-running)/20 px-3.5 py-2.5" : "text-sm sm:pl-7"}>
          <div className={`flex items-center gap-2 text-xs text-muted-foreground ${message.role === "user" ? "mb-1" : "mb-2 sm:-ml-7"}`}>
            {message.role === "assistant" && <span className="inline-flex size-5 items-center justify-center rounded-md bg-muted"><Sparkles className="size-3" /></span>}
            <span className="font-medium text-foreground">{message.role === "user" ? "You" : "Assistant"}</span>
            <time dateTime={new Date(message.createdAt).toISOString()}>{new Date(message.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" })}</time>
          </div>
          {message.role === "user" ? <p className="whitespace-pre-wrap break-words text-sm">{message.content}</p> : (
            <div className="space-y-3 break-words leading-relaxed [&>*:first-child]:mt-0 [&_p]:my-3 [&_p:first-child]:mt-0 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:list-decimal [&_ol]:pl-5 [&_li]:my-1.5 [&_li]:text-muted-foreground [&_li_strong]:text-foreground [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-3 [&_a]:text-primary [&_a]:underline">
              {previous && isAnswers(previous) && <AnswersSummary summary={previous.interview} />}
              {splitContent(message.content || (!message.blocks?.length && !message.interview && message.status === "streaming" ? "Thinking…" : ""), message.blocks).map((part, partIndex) => typeof part === "string" ?
                <ReactMarkdown key={partIndex} remarkPlugins={[remarkGfm]} components={{ img: () => null, a: ({ children, ...props }) => <a {...props} target="_blank" rel="noreferrer">{children}</a> }}>{part}</ReactMarkdown> : part.type === "action" ? <ArtifactActionCard key={part.id} block={part} onStop={streaming ? onCancel : undefined} onRetry={!interviewDisabled && !streaming && index === messages.length - 1 ? onArtifact : undefined} /> : <InlineBlock key={part.id} block={part} onAction={!interviewDisabled && !streaming && message.status === "completed" && index === messages.length - 1 && onArtifact ? (kind) => { if (kind === "artifact") onArtifact() } : undefined} />)}
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
