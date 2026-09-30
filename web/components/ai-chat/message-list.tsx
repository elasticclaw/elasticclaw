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
        <article key={message.id} className={message.role === "user" ? "ml-auto max-w-[85%] rounded-xl border border-primary/25 bg-primary/10 p-3" : "text-sm"}>
          <div className="mb-2 flex items-center gap-2 text-xs text-muted-foreground">
            {message.role === "assistant" && <Sparkles className="size-3.5" />}
            <span className="font-medium text-foreground">{message.role === "user" ? "You" : "Assistant"}</span>
            <time dateTime={new Date(message.createdAt).toISOString()}>{new Date(message.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</time>
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
