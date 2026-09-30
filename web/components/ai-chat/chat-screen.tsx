"use client"

import { Suspense, useEffect, useRef, useState } from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { Sparkles } from "lucide-react"
import { useFeatureFlag, useFeatureFlagsLoaded } from "@/hooks/use-feature-flag"
import { cancelAIChatTurn, createAIChatThread, fetchAIChatSources, fetchAIChatThread, streamAIChatTurn, type AIChatSources, type AIChatThread, type AIChatMessage } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { ChatHeader } from "./header"
import { ModeCards, type ChatMode } from "./mode-cards"
import { ChatComposer } from "./composer"
import { MessageList } from "./message-list"

type TurnError = { message: string; text: string; retry: boolean; afterMessageId?: string }

function reconcileError(error: TurnError | null, messages: AIChatMessage[]): TurnError | null {
  const last = messages[messages.length - 1]
  if (!error || last?.role !== "assistant" || last.id === error.afterMessageId) return error
  if (last.status === "completed") return null
  return ["error", "cancelled", "limit"].includes(last.status) ? { ...error, retry: true } : error
}

export function ChatScreen() {
  const enabled = useFeatureFlag("ai-chat")
  const flagsLoaded = useFeatureFlagsLoaded()
  const router = useRouter()

  useEffect(() => {
    if (flagsLoaded && !enabled) router.replace("/")
  }, [enabled, flagsLoaded, router])

  if (!enabled) return null
  return <Suspense fallback={null}><Conversation /></Suspense>
}

function Conversation() {
  const router = useRouter()
  const params = useSearchParams()
  const requestedThread = params.get("thread") ?? ""
  const threadRef = useRef("")
  const loadedThread = useRef("")
  const active = useRef<AbortController | null>(null)
  const generation = useRef(0)
  const syncRevision = useRef(0)
  const sending = useRef(false)
  const [thread, setThread] = useState<AIChatThread | null>(null)
  const [messages, setMessages] = useState<AIChatMessage[]>([])
  const [draft, setDraft] = useState("")
  const [streaming, setStreaming] = useState(false)
  const [reading, setReading] = useState("")
  const [loadingThread, setLoadingThread] = useState(!!requestedThread)
  const [loadError, setLoadError] = useState("")
  const [turnError, setTurnError] = useState<TurnError | null>(null)

  const [workspace, setWorkspace] = useState("")
  const [mode, setMode] = useState<ChatMode | null>(null)
  const [revision, setRevision] = useState(0)
  const [result, setResult] = useState<{ workspace: string; revision: number; data?: AIChatSources; error?: string }>()
  const current = result?.workspace === workspace && result?.revision === revision ? result : undefined
  const data = current?.data
  const refresh = () => setRevision((value) => value + 1)

  useEffect(() => {
    let cancelled = false
    fetchAIChatSources(workspace).then(
      (data) => {
        if (cancelled) return
        if (workspace && !threadRef.current && !data.workspaces.includes(workspace)) {
          setWorkspace(data.workspaces[0] ?? "")
          setMode(null)
          return
        }
        setResult({ workspace, revision, data })
      },
      () => { if (!cancelled) setResult({ workspace, revision, error: "Unable to load workspace sources." }) },
    )
    return () => { cancelled = true }
  }, [workspace, revision])

  useEffect(() => {
    if (requestedThread === loadedThread.current && (requestedThread || !threadRef.current)) return
    const controller = new AbortController()
    active.current?.abort()
    const version = ++generation.current
    syncRevision.current++
    sending.current = false
    setStreaming(false)
    setReading("")
    setTurnError(null)
    setLoadError("")
    setLoadingThread(!!requestedThread)
    threadRef.current = requestedThread
    loadedThread.current = ""
    setThread(null)
    setMessages([])
    setDraft("")
    setMode(null)
    if (requestedThread) {
      fetchAIChatThread(requestedThread, controller.signal).then(({ thread, messages }) => {
        if (controller.signal.aborted || version !== generation.current) return
        loadedThread.current = thread.id
        setThread(thread)
        setMessages(messages)
        setWorkspace(thread.workspace)
        setMode(thread.mode as ChatMode)
        setLoadingThread(false)
      }, (error: unknown) => {
        if (controller.signal.aborted || version !== generation.current) return
        setLoadError(error instanceof Error ? error.message : "Unable to load conversation.")
        setLoadingThread(false)
      })
    }
    return () => controller.abort()
  }, [requestedThread])

  useEffect(() => () => { generation.current++; active.current?.abort() }, [])

  useEffect(() => {
    const refreshThread = (event: Event) => {
      const id = (event as CustomEvent<{ threadId: string }>).detail.threadId
      if (id !== threadRef.current || sending.current) return
      const version = generation.current
      const revision = ++syncRevision.current
      fetchAIChatThread(id).then((saved) => {
        if (version !== generation.current || revision !== syncRevision.current || sending.current) return
        setThread(saved.thread)
        setMessages(saved.messages)
        setTurnError((error) => reconcileError(error, saved.messages))
      }).catch(() => {})
    }
    window.addEventListener("ai-chat-thread-updated", refreshThread)
    return () => window.removeEventListener("ai-chat-thread-updated", refreshThread)
  }, [])

  const newChat = () => {
    generation.current++
    active.current?.abort()
    sending.current = false
    threadRef.current = ""
    loadedThread.current = ""
    setThread(null)
    setMessages([])
    setMode(null)
    setDraft("")
    setStreaming(false)
    setReading("")
    setTurnError(null)
    setLoadError("")
    setLoadingThread(false)
    router.replace("/chat")
    refresh()
  }

  const send = async (text: string, retry = false) => {
    if (sending.current || streaming || (!retry && !text.trim()) || !data?.configured) return
    sending.current = true
    syncRevision.current++
    const version = generation.current
    const controller = new AbortController()
    active.current = controller
    setStreaming(true)
    setTurnError(null)
    let id = threadRef.current
    let started = false
    let assistantId = ""
    let streamError = ""
    const previousLastId = messages[messages.length - 1]?.id
    try {
      if (!id) {
        const created = await createAIChatThread(data.workspace, mode ?? "explore_idea", controller.signal)
        if (version !== generation.current) return
        id = created.id
        threadRef.current = id
        loadedThread.current = id
        setThread(created)
        setMode(created.mode as ChatMode)
        router.replace(`/chat?thread=${encodeURIComponent(id)}`)
      }
      await streamAIChatTurn(id, text, retry, controller.signal, (event, payload) => {
        if (version !== generation.current) return
        switch (event) {
          case "message_started": {
            if (typeof payload.messageId !== "string") return
            started = true
            assistantId = payload.messageId
            setDraft("")
            const now = Date.now()
            const base = { threadId: id, seq: 0, model: "", inputTokens: 0, outputTokens: 0, createdAt: now }
            setMessages((previous) => [...previous,
              ...(!retry ? [{ ...base, id: `user-${assistantId}`, role: "user" as const, content: text, status: "completed" }] : []),
              { ...base, id: assistantId, role: "assistant", content: "", status: "streaming", model: String(payload.model ?? "") },
            ])
            break
          }
          case "token":
            if (typeof payload.text === "string") setMessages((previous) => previous.map((message) => message.id === assistantId ? { ...message, content: message.content + payload.text } : message))
            break
          case "tool_started": setReading(String(payload.provider || payload.tool || "sources")); break
          case "tool_finished": setReading(""); break
          case "error": streamError = typeof payload.message === "string" ? payload.message : "Unable to complete this turn."; break
          case "done":
            setMessages((previous) => previous.map((message) => message.id === assistantId ? { ...message, status: String(payload.status) } : message))
            break
          // Reserved events and unknown block types are intentionally ignored.
          default: break
        }
      })
      if (streamError) setTurnError({ message: streamError, text, retry: true, afterMessageId: previousLastId })
    } catch (error) {
      if (version === generation.current && !controller.signal.aborted) setTurnError({ message: error instanceof Error ? error.message : "Unable to send message.", text, retry: started || retry, afterMessageId: previousLastId })
    } finally {
      if (version === generation.current) sending.current = false
      if (id && version === generation.current) {
        try {
          const revision = ++syncRevision.current
          const saved = await fetchAIChatThread(id)
          if (version === generation.current && revision === syncRevision.current) {
            setThread(saved.thread)
            setMessages(saved.messages)
            setTurnError((error) => reconcileError(error, saved.messages))
          }
        } catch { /* Keep streamed content if the connection is unavailable. */ }
      }
      if (version === generation.current) {
        sending.current = false
        setStreaming(false)
        setReading("")
        active.current = null
      }
    }
  }

  const cancel = async () => {
    const controller = active.current
    const version = generation.current
    const id = threadRef.current
    // Aborting also covers cancellation before the server acquired a turn slot.
    controller?.abort()
    // The request context cancels this turn. Avoid a delayed /cancel request
    // accidentally cancelling a subsequent turn on the same thread.
    if (controller || !id) return
    try { await cancelAIChatTurn(id) }
    catch {
      if (version === generation.current && controller === active.current) setTurnError({ message: "Connection lost while cancelling. Reload to check the response.", text: "", retry: true })
    }
  }

  const last = messages[messages.length - 1]
  const canRetry = last?.role === "assistant" && ["error", "cancelled", "limit"].includes(last.status)
  const composer = <ChatComposer value={draft} onChange={setDraft} onSend={() => void send(draft)} onCancel={() => void cancel()} streaming={streaming} disabled={!data?.configured || !!thread?.archivedAt} />

  return (
    <section className="flex min-h-0 flex-1 flex-col">
      <ChatHeader mode={mode} title={thread?.title} data={data} onNewChat={newChat} onRefreshSources={refresh} />
      <div className={`min-h-0 flex-1 overflow-y-auto px-8 ${thread ? "py-6" : "py-16"}`}>
        <div className="mx-auto max-w-[860px] space-y-5">
          {!thread && !requestedThread && data && data.workspaces.length > 1 && (
            <div className="flex justify-end gap-2 text-sm">
              <label htmlFor="chat-workspace" className="self-center text-muted-foreground">Workspace</label>
              <select id="chat-workspace" value={data.workspace} onChange={(event) => { setWorkspace(event.target.value); setMode(null) }} className="rounded-md border bg-card px-3 py-1.5">
                {data.workspaces.map((name) => <option key={name} value={name}>{name}</option>)}
              </select>
            </div>
          )}
          {loadingThread ? <p role="status" className="text-sm text-muted-foreground">Loading conversation…</p> : loadError ? (
            <div role="alert" className="space-y-3 text-center"><p>{loadError}</p><Button variant="outline" onClick={() => window.location.reload()}>Retry</Button></div>
          ) : thread ? <MessageList messages={messages} reading={reading} /> : !current ? <p role="status" className="text-center text-sm text-muted-foreground">Loading workspace sources...</p> : current.error ? (
            <div role="alert" className="space-y-3 text-center"><p>{current.error}</p><Button variant="outline" onClick={refresh}>Retry</Button></div>
          ) : !data?.configured ? (
            <div className="rounded-xl border bg-card p-10 text-center">
              <h2 className="text-lg font-semibold">Not configured for this workspace</h2>
              {data?.error && <p role="alert" className="mt-2 text-sm text-destructive">{data.error}</p>}
              <p className="mt-2 text-sm text-muted-foreground">Ask your workspace administrator to configure AI Chat.</p>
            </div>
          ) : (
            <>
              <div className="mb-8 text-center">
                <span className="mb-4 inline-flex size-11 items-center justify-center rounded-xl border bg-card"><Sparkles className="size-5" /></span>
                <h2 className="text-2xl font-semibold tracking-tight">What are we working on?</h2>
                <p className="mt-2 text-sm text-muted-foreground">Pick a mode or just start typing. I read your sources and never change them.</p>
                <div className="mt-3 flex flex-wrap justify-center gap-2">
                  {data.sources.map((source, index) => <span key={`${source.kind}-${index}`} className="flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs text-muted-foreground"><span className={`size-1.5 rounded-full ${source.status === "invalid" ? "bg-destructive" : "bg-muted-foreground"}`} />{source.name}</span>)}
                </div>
              </div>
              <ModeCards mode={mode} onSelect={setMode} />
              {composer}
              <div className="space-y-2">
                <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Suggested</p>
                <div className="flex flex-wrap gap-2">
                  {[
                    "What did clients complain about most this month?",
                    "How is quiz completion doing this month?",
                    "Prototype a simpler check-in screen",
                  ].map((suggestion) => (
                    <Button key={suggestion} variant="outline" size="sm" onClick={() => setDraft(suggestion)} className="rounded-full text-xs">
                      <Sparkles className="size-3" />{suggestion}
                    </Button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      </div>
      {(turnError || (!streaming && canRetry)) && (
        <div role="alert" className="mx-auto flex w-full max-w-[924px] items-center gap-3 px-8 pb-3 text-sm">
          <p className="text-destructive">{turnError?.message ?? "The last response did not finish."}</p>
          <Button variant="outline" size="sm" disabled={streaming} onClick={() => void send(turnError?.text ?? "", turnError?.retry ?? true)}>Retry</Button>
        </div>
      )}
      {thread && <div className="mx-auto w-full max-w-[924px] px-8 pb-5">
        {thread.archivedAt && <p className="mb-2 text-xs text-muted-foreground">This conversation is archived.</p>}
        {!data?.configured && <p className="mb-2 text-xs text-muted-foreground">{!current ? "Loading workspace sources…" : "Not configured for this workspace"}</p>}
        {composer}
      </div>}
    </section>
  )
}
