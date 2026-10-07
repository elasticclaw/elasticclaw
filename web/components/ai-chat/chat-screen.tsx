"use client"

import { useEffect, useState } from "react"
import { useRouter } from "next/navigation"
import { Sparkles } from "lucide-react"
import { useFeatureFlag, useFeatureFlagsLoaded } from "@/hooks/use-feature-flag"
import { fetchAIChatSources, type AIChatSources } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { ChatHeader } from "./header"
import { ModeCards, type ChatMode } from "./mode-cards"
import { ChatComposer } from "./composer"

export function ChatScreen() {
  const enabled = useFeatureFlag("ai-chat")
  const flagsLoaded = useFeatureFlagsLoaded()
  const router = useRouter()

  useEffect(() => {
    if (flagsLoaded && !enabled) router.replace("/")
  }, [enabled, flagsLoaded, router])

  if (!enabled) return null
  return <NewChat />
}

function NewChat() {
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
        if (workspace && !data.workspaces.includes(workspace)) {
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

  return (
    <section className="flex min-h-0 flex-1 flex-col">
      <ChatHeader mode={mode} data={data} onNewChat={() => { setMode(null); refresh() }} onRefreshSources={refresh} />
      <div className="flex-1 overflow-y-auto px-8 py-16">
        <div className="mx-auto max-w-[860px] space-y-5">
          {data && data.workspaces.length > 1 && (
            <div className="flex justify-end gap-2 text-sm">
              <label htmlFor="chat-workspace" className="self-center text-muted-foreground">Workspace</label>
              <select id="chat-workspace" value={data.workspace} onChange={(event) => { setWorkspace(event.target.value); setMode(null) }} className="rounded-md border bg-card px-3 py-1.5">
                {data.workspaces.map((name) => <option key={name} value={name}>{name}</option>)}
              </select>
            </div>
          )}
          {!current ? <p role="status" className="text-center text-sm text-muted-foreground">Loading workspace sources...</p> : current.error ? (
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
              <ChatComposer />
              <div className="space-y-2">
                <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Suggested</p>
                <div className="flex flex-wrap gap-2">
                  {[
                    "What did clients complain about most this month?",
                    "How is quiz completion doing this month?",
                    "Prototype a simpler check-in screen",
                  ].map((suggestion) => (
                    <Button key={suggestion} variant="outline" disabled className="rounded-full px-3.5">
                      <Sparkles className="size-3.5" />{suggestion}
                    </Button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      </div>
    </section>
  )
}
