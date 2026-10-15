"use client"

import { useEffect, useEffectEvent, useRef, useState } from "react"
import { useRouter } from "next/navigation"
import { Eye, FileCode2, History, Ticket, Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { deleteAIChatArtifact, fetchAIChatArtifacts, fetchAIChatThreads, type AIChatArtifactListItem, type AIChatThread } from "@/lib/api"
import { chatModes } from "./mode-cards"

// The caller captures the clock when opening, never during render/static export.
export function historyTime(timestamp: number, now: number) {
  const date = new Date(timestamp)
  const today = new Date(now)
  today.setHours(0, 0, 0, 0)
  const week = new Date(today)
  week.setDate(week.getDate() - 6)
  if (timestamp >= today.getTime()) return { group: "Today", label: date.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" }) }
  if (timestamp >= week.getTime()) return { group: "Last 7 days", label: date.toLocaleDateString("en-US", { weekday: "short" }) }
  return { group: "Older", label: date.toLocaleDateString("en-US", { month: "short", day: "numeric", ...(date.getFullYear() !== today.getFullYear() ? { year: "numeric" as const } : {}) }) }
}

export function HistoryPopover({ threadId }: { threadId?: string }) {
  const router = useRouter()
  const [open, setOpen] = useState(false)
  const [now, setNow] = useState(0)
  const [tab, setTab] = useState("chats")
  const [chats, setChats] = useState<AIChatThread[]>([])
  const [artifacts, setArtifacts] = useState<AIChatArtifactListItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [confirm, setConfirm] = useState("")
  const [deleting, setDeleting] = useState("")
  const request = useRef<AbortController | null>(null)
  const removePending = useRef(false)

  const refresh = async () => {
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    setLoading(true)
    setError("")
    try {
      const [threads, items] = await Promise.all([fetchAIChatThreads(controller.signal), fetchAIChatArtifacts(controller.signal)])
      if (controller.signal.aborted) return
      setChats(threads)
      setArtifacts(items)
    } catch {
      if (!controller.signal.aborted) setError("Unable to load history. Try again.")
    } finally {
      if (!controller.signal.aborted) setLoading(false)
    }
  }
  const onUpdated = useEffectEvent(() => { if (open && !removePending.current) void refresh() })
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined
    const listener = () => { clearTimeout(timer); timer = setTimeout(onUpdated, 300) }
    window.addEventListener("ai-chat-thread-updated", listener)
    return () => { clearTimeout(timer); request.current?.abort(); window.removeEventListener("ai-chat-thread-updated", listener) }
  }, [])

  const changeOpen = (value: boolean) => {
    setOpen(value)
    setConfirm("")
    if (value) void refresh()
    else { request.current?.abort(); setLoading(false) }
  }
  const openChat = (id: string) => { changeOpen(false); router.replace(`/chat?thread=${encodeURIComponent(id)}`) }
  const remove = async (item: AIChatArtifactListItem) => {
    if (removePending.current) return
    removePending.current = true
    setDeleting(item.id)
    setError("")
    request.current?.abort()
    setLoading(false)
    try {
      await deleteAIChatArtifact(item.slug)
      setArtifacts((items) => items.filter((value) => value.id !== item.id))
      setChats((items) => items.map((value) => value.id === item.threadId ? { ...value, artifactCount: Math.max(0, (value.artifactCount ?? 0) - 1) } : value))
      setConfirm("")
      window.dispatchEvent(new CustomEvent("ai-chat-thread-updated", { detail: { threadId: item.threadId } }))
    } catch { setError("Unable to delete artifact. Try again.") }
    finally { removePending.current = false; setDeleting("") }
  }

  // Radix calls onOpenChange from interaction handlers, never during render.
  // eslint-disable-next-line react-hooks/purity
  return <Popover open={open} onOpenChange={(value) => { if (value) setNow(Date.now()); changeOpen(value) }}>
    <PopoverTrigger asChild><Button variant="outline" size="sm" className="text-[13px]"><History className="size-3.5" />History</Button></PopoverTrigger>
    <PopoverContent align="end" sideOffset={6} className="w-[420px] max-w-[calc(100vw-2rem)] rounded-xl p-2.5" aria-label="History">
      <Tabs value={tab} onValueChange={(value) => { setTab(value); setConfirm("") }}>
        <TabsList className="w-full"><TabsTrigger value="chats">Chats</TabsTrigger><TabsTrigger value="artifacts">Artifacts</TabsTrigger></TabsList>
        {loading && <p role="status" className="p-3 text-xs text-muted-foreground">Loading history…</p>}
        {error && <div role="alert" className="p-3 text-xs text-destructive">{error}<Button variant="ghost" size="sm" onClick={() => void refresh()}>Retry</Button></div>}
        <TabsContent value="chats" className="max-h-[min(60vh,480px)] overflow-y-auto">
          {!loading && !error && chats.length === 0 && <p className="p-3 text-sm text-muted-foreground">No chats yet.</p>}
          {["Today", "Last 7 days", "Older"].map((group) => {
            const items = chats.filter((item) => historyTime(item.updatedAt, now).group === group)
            return items.length > 0 && <section key={group}>
              <h3 className="px-2.5 pt-3 pb-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{group}</h3>
              {items.map((item) => {
                const mode = chatModes.find((mode) => mode.id === item.mode)
                const Icon = mode?.icon ?? History
                return <button key={item.id} type="button" aria-current={item.id === threadId ? "page" : undefined} onClick={() => openChat(item.id)} className={`flex w-full items-center gap-2.5 rounded-lg p-2.5 text-left hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${item.id === threadId ? "bg-accent" : ""}`}>
                  <span className="rounded-md bg-muted p-1.5"><Icon className="size-3.5 text-muted-foreground" /></span>
                  <span className="min-w-0 flex-1"><span className="block truncate text-[13px] font-medium">{item.title || "New chat"}</span><span className="block text-xs text-muted-foreground">{mode?.name ?? item.mode} · {historyTime(item.updatedAt, now).label}</span></span>
                  {!!item.artifactCount && <span aria-label={`${item.artifactCount} artifacts`} className="inline-flex items-center gap-1 rounded-full border px-1.5 py-0.5 text-[10px] text-muted-foreground"><FileCode2 className="size-3" />{item.artifactCount}</span>}
                  {item.ticket && <span className="inline-flex items-center gap-1 rounded-full border px-1.5 py-0.5 text-[10px] text-muted-foreground"><Ticket className="size-3" />{item.ticket}</span>}
                </button>
              })}
            </section>
          })}
        </TabsContent>
        <TabsContent value="artifacts" className="max-h-[min(60vh,480px)] overflow-y-auto">
          {!loading && !error && artifacts.length === 0 && <p className="p-3 text-sm text-muted-foreground">No artifacts yet. Ask for a prototype in any chat.</p>}
          {["Today", "Last 7 days", "Older"].map((group) => {
            const items = artifacts.filter((item) => historyTime(item.updatedAt, now).group === group)
            return items.length > 0 && <section key={group}>
              <h3 className="px-2.5 pt-3 pb-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{group}</h3>
              {items.map((item) => <div key={item.id} className="rounded-lg p-2.5 hover:bg-accent">
                <div className="flex items-center gap-2.5">
                  <FileCode2 className="size-4 shrink-0 text-muted-foreground" />
                  <a href={item.url} target="_blank" rel="noreferrer" className="min-w-0 flex-1 rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"><span className="block truncate text-[13px] font-medium">{item.title}</span><span className="text-xs text-muted-foreground">v{item.currentVersion} · {historyTime(item.updatedAt, now).label}</span></a>
                  <Button variant="ghost" size="icon" className="size-7" aria-label={`Delete ${item.title}`} disabled={!!deleting} onClick={() => setConfirm(item.id)}><Trash2 className="size-3.5" /></Button>
                </div>
                <Button variant="link" size="sm" className="ml-6 h-6 px-0 text-xs text-muted-foreground" title={item.threadTitle} onClick={() => openChat(item.threadId)}>Open chat</Button>
                {confirm === item.id && <div className="mt-2 rounded-md border p-2 text-xs"><p>Delete this artifact? The link stops working.</p><div className="mt-2 flex justify-end gap-2"><Button variant="outline" size="sm" disabled={!!deleting} onClick={() => setConfirm("")}>Cancel</Button><Button variant="destructive" size="sm" disabled={!!deleting} onClick={() => void remove(item)}>{deleting === item.id ? "Deleting…" : "Delete"}</Button></div></div>}
              </div>)}
            </section>
          })}
        </TabsContent>
      </Tabs>
      <p className="mt-2 flex items-center gap-1.5 border-t px-2.5 pt-3 pb-1 text-[11px] text-muted-foreground"><Eye className="size-3" />Only you can see your {tab === "chats" ? "chats" : "artifacts"}.</p>
    </PopoverContent>
  </Popover>
}
