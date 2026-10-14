"use client"

import Link from "next/link"
import { useEffect, useRef, useState } from "react"
import { Download, FileCode2, Loader2 } from "lucide-react"
import { ApiError, fetchAIChatArtifact, type AIChatArtifact } from "@/lib/api"
import { requestAuthToken } from "@/lib/auth-storage"
import { getHubUrl } from "@/lib/hub-url"
import { artifactLocation, artifactPageError, artifactShareURL, artifactURLExpired } from "@/lib/ai-chat-artifacts"
import { CopyArtifactLink } from "@/components/ai-chat/artifact-card"
import { Button } from "@/components/ui/button"

export default function ArtifactPage() {
  const [artifact, setArtifact] = useState<AIChatArtifact | null>(null)
  const [error, setError] = useState("")
  const [attempt, setAttempt] = useState(0)
  const current = useRef<AIChatArtifact | null>(null)
  useEffect(() => {
    const controller = new AbortController()
    const location = artifactLocation(window.location.pathname)
    let pending = false
    const load = async () => {
      if (pending) return
      pending = true
      try {
        if (!location) { setError(artifactPageError(404)); return }
        await requestAuthToken()
        if (controller.signal.aborted) return
        const value = await fetchAIChatArtifact(location.slug, location.version, controller.signal)
        if (!controller.signal.aborted) { current.current = value; setArtifact(value); setError("") }
      } catch (cause) {
        if (!controller.signal.aborted) { setArtifact(null); setError(artifactPageError(cause instanceof ApiError ? cause.status : undefined)) }
      } finally { pending = false }
    }
    const focus = () => { if (!current.current || artifactURLExpired(current.current.rawUrl)) void load() }
    void load()
    window.addEventListener("focus", focus)
    return () => { controller.abort(); window.removeEventListener("focus", focus) }
  }, [attempt])

  if (error) return <main className="flex h-full flex-col items-center justify-center gap-4 p-8"><p role="alert">{error}</p><Button variant="outline" onClick={() => setAttempt((value) => value + 1)}>Try again</Button><Link className="text-sm text-muted-foreground" href="/">Back to the hub</Link></main>
  if (!artifact) return <main role="status" className="flex h-full items-center justify-center gap-2 text-muted-foreground"><Loader2 className="size-4 animate-spin" />Loading artifact…</main>
  const share = artifactShareURL(artifact.slug, artifact.shareUrl)
  const pinned = artifactLocation(window.location.pathname)?.version !== undefined
  return <main className="flex h-full flex-col">
    <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b bg-card px-5 py-3 text-xs">
      <div className="flex min-w-0 items-center gap-3"><Link className="shrink-0 text-sm font-semibold" href="/">Faster Factory</Link><span className="text-muted-foreground">/</span><FileCode2 className="size-3.5 shrink-0 text-muted-foreground" /><h1 className="truncate text-sm font-semibold">{artifact.title}</h1></div>
      <div className="text-muted-foreground">v{artifact.version}{pinned && ` of ${artifact.currentVersion}`} · by {artifact.ownerLogin} · from AI Chat · {new Date(artifact.createdAt).toLocaleDateString(undefined, { month: "short", day: "numeric" })}{pinned && <a className="ml-2 underline" href={share}>Latest version</a>}</div>
      <div className="ml-auto flex flex-wrap items-center gap-2"><span className="max-w-96 truncate font-mono text-muted-foreground">{share}</span><CopyArtifactLink url={share} /><Button variant="outline" size="sm" asChild><a href={`${getHubUrl()}${artifact.downloadUrl}`}><Download className="size-3.5" />Download .html</a></Button></div>
    </header>
    <iframe className="min-h-0 w-full flex-1 border-0 bg-white" src={`${getHubUrl()}${artifact.rawUrl}`} sandbox="allow-scripts" referrerPolicy="no-referrer" title={artifact.title} />
  </main>
}
