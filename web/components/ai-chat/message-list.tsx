"use client"

import { useEffect, useRef } from "react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { Sparkles } from "lucide-react"
import type { AIChatMessage } from "@/lib/api"

export function MessageList({ messages, reading }: { messages: AIChatMessage[]; reading: string }) {
  const end = useRef<HTMLDivElement>(null)
  useEffect(() => { end.current?.scrollIntoView({ block: "end" }) }, [messages, reading])
  return (
    <div className="space-y-6" role="log" aria-label="Conversation">
      {messages.map((message) => (
        <article key={message.id} className={message.role === "user" ? "ml-auto w-fit max-w-[85%] rounded-lg border border-(--step-running)/50 bg-(--step-running)/20 px-3.5 py-2.5" : "pl-7 text-sm"}>
          <div className={`flex items-center gap-2 text-xs text-muted-foreground ${message.role === "user" ? "mb-1" : "-ml-7 mb-2"}`}>
            {message.role === "assistant" && <span className="inline-flex size-5 items-center justify-center rounded-md bg-muted"><Sparkles className="size-3" /></span>}
            <span className="font-medium text-foreground">{message.role === "user" ? "You" : "Assistant"}</span>
            <time dateTime={new Date(message.createdAt).toISOString()}>{new Date(message.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" })}</time>
          </div>
          {message.role === "user" ? <p className="whitespace-pre-wrap break-words text-sm">{message.content}</p> : (
            <div className="space-y-3 break-words leading-relaxed [&_p]:my-3 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:list-decimal [&_ol]:pl-5 [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-3 [&_a]:text-primary [&_a]:underline">
              <ReactMarkdown remarkPlugins={[remarkGfm]} components={{ img: () => null, a: ({ children, ...props }) => <a {...props} target="_blank" rel="noreferrer">{children}</a> }}>{message.content || (message.status === "streaming" ? "Thinking…" : "")}</ReactMarkdown>
            </div>
          )}
          {message.status === "cancelled" && <p className="mt-2 text-xs text-muted-foreground">Response cancelled.</p>}
          {(message.status === "error" || message.status === "limit") && <p className="mt-2 text-xs text-destructive">This response did not finish.</p>}
        </article>
      ))}
      {reading && <p role="status" className="text-xs text-muted-foreground">Reading {reading}…</p>}
      <div ref={end} />
    </div>
  )
}
