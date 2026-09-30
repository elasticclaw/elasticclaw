"use client"

import { useEffect, useEffectEvent, useRef, useState } from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { cancelAIChatTurn, createAIChatThread, fetchAIChatThread, streamAIChatTurn, type AIChatConversation, type AIChatThread, type AIChatMessage } from "@/lib/api"
import type { ChatMode } from "./mode-cards"
import { applyStreamEvent, type StreamTurn } from "./stream-turn"
import { useChatSources } from "./use-chat-sources"

type TurnError = { message: string; text: string; retry: boolean; afterMessageId?: string }

const unfinishedStatuses = ["error", "cancelled", "limit"]

function isUnfinishedReply(message: AIChatMessage | undefined) {
  return message?.role === "assistant" && unfinishedStatuses.includes(message.status)
}

function reconcileError(error: TurnError | null, messages: AIChatMessage[]): TurnError | null {
  const last = messages[messages.length - 1]
  if (!error || last?.role !== "assistant" || last.id === error.afterMessageId) return error
  if (last.status === "completed") return null
  return isUnfinishedReply(last) ? { ...error, retry: true } : error
}

function errorMessage(error: unknown, fallback: string) {
  return error instanceof Error ? error.message : fallback
}

// Owns the open conversation: loading a thread, cross-tab sync, and sending or
// cancelling turns. Every async result is checked against the stale-response
// guards below before it touches the screen.
export function useConversation() {
  const router = useRouter()
  const requestedThread = useSearchParams().get("thread") ?? ""
  const threadRef = useRef("")
  const loadedThread = useRef("")
  // Guards against stale responses: `generation` changes whenever the visible
  // conversation is replaced; `syncRevision` whenever a newer thread sync or
  // turn starts.
  const active = useRef<AbortController | null>(null)
  const generation = useRef(0)
  const syncRevision = useRef(0)
  // The syncRevision of the last thread sync shown on screen.
  const shownSync = useRef(0)
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

  const isSameConversation = (version: number) => version === generation.current
  const isLatestSync = (revision: number) => revision === syncRevision.current
  const hasNewerSyncShown = (revision: number) => shownSync.current > revision
  const isAlreadyOpen = (id: string) => id === loadedThread.current && (!!id || !threadRef.current)

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

  const syncThread = async (id: string) => {
    const version = generation.current
    const revision = ++syncRevision.current
    const saved = await fetchAIChatThread(id)
    if (!isSameConversation(version) || !isLatestSync(revision) || sending.current) return
    shownSync.current = revision
    setThread(saved.thread)
    setMessages(saved.messages)
    setTurnError((error) => reconcileError(error, saved.messages))
  }

  const showLoadedThread = ({ thread, messages }: AIChatConversation, revision: number) => {
    loadedThread.current = thread.id
    if (!hasNewerSyncShown(revision)) {
      setThread(thread)
      setMessages(messages)
    }
    setWorkspace(thread.workspace)
    setMode(thread.mode as ChatMode)
    setLoadingThread(false)
  }

  const showLoadError = (error: unknown) => {
    setLoadError(errorMessage(error, "Unable to load conversation."))
    setLoadingThread(false)
  }

  const openThread = useEffectEvent((id: string) => {
    if (isAlreadyOpen(id)) return
    const version = resetConversation(id)
    if (!id) return
    const controller = new AbortController()
    const isCurrent = () => !controller.signal.aborted && isSameConversation(version)
    const revision = syncRevision.current
    fetchAIChatThread(id, controller.signal).then(
      (loaded) => { if (isCurrent()) showLoadedThread(loaded, revision) },
      (error: unknown) => { if (isCurrent()) showLoadError(error) },
    )
    return () => controller.abort()
  })

  // Another tab (or the sidebar) changed this thread.
  const onThreadUpdated = useEffectEvent((id: string) => {
    if (id === threadRef.current && !sending.current) syncThread(id).catch(() => {})
  })

  // Navigating to another thread must drop the visible one before loading it.
  // eslint-disable-next-line react-hooks/set-state-in-effect
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

  const selectWorkspace = (workspace: string) => {
    setWorkspace(workspace)
    setMode(null)
  }

  const openCreatedThread = (created: AIChatThread) => {
    threadRef.current = created.id
    loadedThread.current = created.id
    setThread(created)
    setMode(created.mode as ChatMode)
    router.replace(`/chat?thread=${encodeURIComponent(created.id)}`)
  }

  const canStartTurn = (text: string, retry: boolean) => !sending.current && !streaming && (retry || !!text.trim())

  const startTurn = () => {
    sending.current = true
    syncRevision.current++
    const controller = new AbortController()
    active.current = controller
    setStreaming(true)
    setTurnError(null)
    return controller
  }

  // Creates the thread on the first message, then streams the reply into it.
  const streamTurn = async (turn: StreamTurn, workspace: string, signal: AbortSignal, isCurrent: () => boolean) => {
    if (!turn.threadId) {
      const created = await createAIChatThread(workspace, mode ?? "explore_idea", signal)
      if (!isCurrent()) return
      openCreatedThread(created)
      turn.threadId = created.id
    }
    await streamAIChatTurn(turn.threadId, turn.text, turn.retry, signal, (event, payload) => {
      if (isCurrent()) applyStreamEvent(turn, event, payload, { setMessages, setDraft, setReading })
    })
  }

  const finishTurn = async (turn: StreamTurn, isCurrent: () => boolean) => {
    if (isCurrent()) sending.current = false
    // Keep streamed content if the connection is unavailable.
    if (turn.threadId && isCurrent()) await syncThread(turn.threadId).catch(() => {})
    if (!isCurrent()) return
    sending.current = false
    setStreaming(false)
    setReading("")
    active.current = null
  }

  const send = async (text: string, retry = false) => {
    if (!data?.configured || !canStartTurn(text, retry)) return
    const controller = startTurn()
    const version = generation.current
    const isCurrent = () => isSameConversation(version)
    const turn: StreamTurn = { threadId: threadRef.current, text, retry, started: false, assistantId: "", error: "" }
    const afterMessageId = messages[messages.length - 1]?.id
    try {
      await streamTurn(turn, data.workspace, controller.signal, isCurrent)
      if (turn.error) setTurnError({ message: turn.error, text, retry: true, afterMessageId })
    } catch (error) {
      if (isCurrent() && !controller.signal.aborted) setTurnError({ message: errorMessage(error, "Unable to send message."), text, retry: turn.started || retry, afterMessageId })
    } finally {
      await finishTurn(turn, isCurrent)
    }
  }

  const retryTurn = () => send(turnError?.text ?? "", turnError?.retry ?? true)

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
      if (isSameConversation(version) && controller === active.current) setTurnError({ message: "Connection lost while cancelling. Reload to check the response.", text: "", retry: true })
    }
  }

  const canRetry = isUnfinishedReply(messages[messages.length - 1])

  return {
    requestedThread, thread, messages, draft, setDraft, streaming, reading, loadingThread, loadError, turnError, canRetry,
    mode, setMode, current, data, selectWorkspace, refresh, newChat, send, retryTurn, cancel,
  }
}
