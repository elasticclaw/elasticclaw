"use client"

import { Suspense, useEffect, useEffectEvent, useRef, useState } from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { Sparkles } from "lucide-react"
import { useFeatureFlag, useFeatureFlagsLoaded } from "@/hooks/use-feature-flag"
import { cancelAIChatTurn, createAIChatThread, fetchAIChatThread, streamAIChatTurn, type AIChatConversation, type AIChatThread, type AIChatMessage } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { ChatHeader } from "./header"
import { ModeCards, type ChatMode } from "./mode-cards"
import { ChatComposer } from "./composer"
import { MessageList } from "./message-list"
import { applyStreamEvent, type StreamTurn } from "./stream-turn"
import { useChatSources } from "./use-chat-sources"

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
  // Guards against stale responses: `generation` changes whenever the visible
  // conversation is replaced; `syncRevision` whenever a newer thread sync or
  // turn starts.
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
  const [mode, setMode] = useState<ChatMode | null>(null)
  const { current, data, setWorkspace, refresh } = useChatSources(threadRef, () => setMode(null))

  // Drops the visible conversation and invalidates every in-flight request.
  const resetConversation = (threadId = "") => {
    active.current?.abort()
    const version = ++generation.current
    syncRevision.current++
    sending.current = false
    threadRef.current = threadId
    loadedThread.current = ""
    setThread(null)
    setMessages([])
    setDraft("")
    setMode(null)
    setStreaming(false)
    setReading("")
    setTurnError(null)
    setLoadError("")
    setLoadingThread(!!threadId)
    return version
  }

  const showSavedThread = (saved: AIChatConversation) => {
    setThread(saved.thread)
    setMessages(saved.messages)
    setTurnError((error) => reconcileError(error, saved.messages))
  }

  const syncThread = async (id: string) => {
    const version = generation.current
    const revision = ++syncRevision.current
    const saved = await fetchAIChatThread(id)
    if (version === generation.current && revision === syncRevision.current && !sending.current) showSavedThread(saved)
  }

  const openThread = useEffectEvent((id: string) => {
    if (id === loadedThread.current && (id || !threadRef.current)) return
    const version = resetConversation(id)
    if (!id) return
    const controller = new AbortController()
    const isCurrent = () => !controller.signal.aborted && version === generation.current
    fetchAIChatThread(id, controller.signal).then(({ thread, messages }) => {
      if (!isCurrent()) return
      loadedThread.current = thread.id
      setThread(thread)
      setMessages(messages)
      setWorkspace(thread.workspace)
      setMode(thread.mode as ChatMode)
      setLoadingThread(false)
    }, (error: unknown) => {
      if (!isCurrent()) return
      setLoadError(error instanceof Error ? error.message : "Unable to load conversation.")
      setLoadingThread(false)
    })
    return () => controller.abort()
  })

  // Another tab (or the sidebar) changed this thread.
  const onThreadUpdated = useEffectEvent((id: string) => {
    if (id === threadRef.current && !sending.current) syncThread(id).catch(() => {})
  })

  useEffect(() => openThread(requestedThread), [requestedThread])

  useEffect(() => () => { generation.current++; active.current?.abort() }, [])

  useEffect(() => {
    const listener = (event: Event) => onThreadUpdated((event as CustomEvent<{ threadId: string }>).detail.threadId)
    window.addEventListener("ai-chat-thread-updated", listener)
    return () => window.removeEventListener("ai-chat-thread-updated", listener)
  }, [])

  const newChat = () => {
    resetConversation()
    router.replace("/chat")
    refresh()
  }

  const openCreatedThread = (created: AIChatThread) => {
    threadRef.current = created.id
    loadedThread.current = created.id
    setThread(created)
    setMode(created.mode as ChatMode)
    router.replace(`/chat?thread=${encodeURIComponent(created.id)}`)
  }

  const send = async (text: string, retry = false) => {
    if (sending.current || streaming || (!retry && !text.trim()) || !data?.configured) return
    sending.current = true
    syncRevision.current++
    const version = generation.current
    const isCurrent = () => version === generation.current
    const controller = new AbortController()
    active.current = controller
    setStreaming(true)
    setTurnError(null)
    const turn: StreamTurn = { threadId: threadRef.current, text, retry, started: false, assistantId: "", error: "" }
    const afterMessageId = messages[messages.length - 1]?.id
    try {
      if (!turn.threadId) {
        const created = await createAIChatThread(data.workspace, mode ?? "explore_idea", controller.signal)
        if (!isCurrent()) return
        openCreatedThread(created)
        turn.threadId = created.id
      }
      await streamAIChatTurn(turn.threadId, text, retry, controller.signal, (event, payload) => {
        if (isCurrent()) applyStreamEvent(turn, event, payload, { setMessages, setDraft, setReading })
      })
      if (turn.error) setTurnError({ message: turn.error, text, retry: true, afterMessageId })
    } catch (error) {
      if (isCurrent() && !controller.signal.aborted) setTurnError({ message: error instanceof Error ? error.message : "Unable to send message.", text, retry: turn.started || retry, afterMessageId })
    } finally {
      if (isCurrent()) sending.current = false
      // Keep streamed content if the connection is unavailable.
      if (turn.threadId && isCurrent()) await syncThread(turn.threadId).catch(() => {})
      if (isCurrent()) {
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
        <div className={`mx-auto space-y-5 ${thread ? "max-w-[760px]" : "max-w-[860px]"}`}>
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
                    <Button key={suggestion} variant="outline" onClick={() => setDraft(suggestion)} className="rounded-full px-3.5">
                      <Sparkles className="size-3.5" />{suggestion}
                    </Button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      </div>
      {(turnError || (!streaming && canRetry)) && (
        <div role="alert" className="mx-auto flex w-full max-w-[824px] items-center gap-3 px-8 pb-3 text-sm">
          <p className="text-destructive">{turnError?.message ?? "The last response did not finish."}</p>
          <Button variant="outline" size="sm" disabled={streaming} onClick={() => void send(turnError?.text ?? "", turnError?.retry ?? true)}>Retry</Button>
        </div>
      )}
      {thread && <div className="mx-auto w-full max-w-[824px] px-8 pb-5">
        {thread.archivedAt && <p className="mb-2 text-xs text-muted-foreground">This conversation is archived.</p>}
        {!data?.configured && <p className="mb-2 text-xs text-muted-foreground">{!current ? "Loading workspace sources…" : "Not configured for this workspace"}</p>}
        {composer}
      </div>}
    </section>
  )
}
