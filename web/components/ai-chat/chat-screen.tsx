"use client"

import { Suspense, useEffect, type ReactNode } from "react"
import { useRouter } from "next/navigation"
import { Sparkles } from "lucide-react"
import { useFeatureFlag, useFeatureFlagsLoaded } from "@/hooks/use-feature-flag"
import { Button } from "@/components/ui/button"
import { ChatHeader } from "./header"
import { ModeCards, type ChatMode } from "./mode-cards"
import { ChatComposer } from "./composer"
import { MessageList } from "./message-list"
import { useConversation } from "./use-conversation"
import type { AIChatMessage, AIChatSources, AIChatThread } from "@/lib/api"

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
  const {
    requestedThread, thread, messages, draft, setDraft, streaming, reading, loadingThread, loadError, turnError, canRetry,
    mode, setMode, current, data, selectWorkspace, refresh, newChat, send, retryTurn, cancel,
  } = useConversation()
  const composer = <ChatComposer value={draft} onChange={setDraft} onSend={() => void send(draft)} onCancel={() => void cancel()} streaming={streaming} disabled={!data?.configured || !!thread?.archivedAt} />

  return (
    <section className="flex min-h-0 flex-1 flex-col">
      <ChatHeader mode={mode} title={thread?.title} data={data} onNewChat={newChat} onRefreshSources={refresh} />
      <div className={`min-h-0 flex-1 overflow-y-auto px-8 ${thread ? "py-6" : "py-16"}`}>
        <div className={`mx-auto space-y-5 ${thread ? "max-w-[760px]" : "max-w-[860px]"}`}>
          {!thread && !requestedThread && data && data.workspaces.length > 1 && renderWorkspaceSelect(data, selectWorkspace)}
          {renderBody({ loadingThread, loadError, thread, messages, reading, current, data, mode, setMode, setDraft, refresh, composer })}
        </div>
      </div>
      {(turnError || (!streaming && canRetry)) && (
        <div role="alert" className="mx-auto flex w-full max-w-[824px] items-center gap-3 px-8 pb-3 text-sm">
          <p className="text-destructive">{turnError?.message ?? "The last response did not finish."}</p>
          <Button variant="outline" size="sm" disabled={streaming} onClick={() => void retryTurn()}>Retry</Button>
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

const suggestions = [
  "What did clients complain about most this month?",
  "How is quiz completion doing this month?",
  "Prototype a simpler check-in screen",
]

type BodyState = {
  loadingThread: boolean
  loadError: string
  thread: AIChatThread | null
  messages: AIChatMessage[]
  reading: string
  current?: { error?: string }
  data?: AIChatSources
  mode: ChatMode | null
  setMode: (mode: ChatMode) => void
  setDraft: (draft: string) => void
  refresh: () => void
  composer: ReactNode
}

// Plain render helpers (not components) keep the element tree flat for tests.
function renderBody({ loadingThread, loadError, thread, messages, reading, current, data, refresh, ...newChat }: BodyState) {
  if (loadingThread) return <p role="status" className="text-sm text-muted-foreground">Loading conversation…</p>
  if (loadError) return renderRetry(loadError, () => window.location.reload())
  if (thread) return <MessageList messages={messages} reading={reading} />
  if (!current) return <p role="status" className="text-center text-sm text-muted-foreground">Loading workspace sources...</p>
  if (current.error) return renderRetry(current.error, refresh)
  if (!data?.configured) return renderNotConfigured(data?.error)
  return renderNewChat({ data, ...newChat })
}

function renderWorkspaceSelect(data: AIChatSources, onSelect: (workspace: string) => void) {
  return (
    <div className="flex justify-end gap-2 text-sm">
      <label htmlFor="chat-workspace" className="self-center text-muted-foreground">Workspace</label>
      <select id="chat-workspace" value={data.workspace} onChange={(event) => onSelect(event.target.value)} className="rounded-md border bg-card px-3 py-1.5">
        {data.workspaces.map((name) => <option key={name} value={name}>{name}</option>)}
      </select>
    </div>
  )
}

function renderRetry(message: string, onRetry: () => void) {
  return <div role="alert" className="space-y-3 text-center"><p>{message}</p><Button variant="outline" onClick={onRetry}>Retry</Button></div>
}

function renderNotConfigured(error?: string) {
  return (
    <div className="rounded-xl border bg-card p-10 text-center">
      <h2 className="text-lg font-semibold">Not configured for this workspace</h2>
      {error && <p role="alert" className="mt-2 text-sm text-destructive">{error}</p>}
      <p className="mt-2 text-sm text-muted-foreground">Ask your workspace administrator to configure AI Chat.</p>
    </div>
  )
}

function renderNewChat({ data, mode, setMode, setDraft, composer }: Pick<BodyState, "mode" | "setMode" | "setDraft" | "composer"> & { data: AIChatSources }) {
  return (
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
          {suggestions.map((suggestion) => (
            <Button key={suggestion} variant="outline" onClick={() => setDraft(suggestion)} className="rounded-full px-3.5">
              <Sparkles className="size-3.5" />{suggestion}
            </Button>
          ))}
        </div>
      </div>
    </>
  )
}
