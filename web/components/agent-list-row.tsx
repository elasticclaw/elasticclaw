"use client"

import { Pin } from "lucide-react"
import { useSortable } from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"
import { cn } from "@/lib/utils"
import { COLOR_CLASSES } from "@/lib/mappers"
import type { Claw, Message } from "@/lib/types"
import { BootstrapProgress } from "@/components/bootstrap-progress"
import { ClawTitle } from "@/components/claw-title"
import { StatusDot, formatUptime } from "@/components/chat-view"

export function lastSnippet(messages: Message[]) { const message = [...messages].reverse().find((item) => (item.role === "user" || item.role === "claw") && item.content !== "__THINKING__" && !item.id.startsWith("streaming-")); return message ? message.content.split("\n").find(Boolean)?.replace(/^[#>*`\-\s]+/, "") || "No messages yet" : "No messages yet" }
export function formatRelativeTime(date?: Date) { if (!date) return ""; const days = Math.floor((Date.now() - new Date(date).getTime()) / 86400000); if (days === 0) return new Date(date).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }); return days < 7 ? `${days}d` : new Date(date).toLocaleDateString([], { month: "short", day: "numeric" }) }

type Sortable = ReturnType<typeof useSortable>
type Props = { claw: Claw; isSelected: boolean; messages: Message[]; onClick: () => void; onTogglePin: () => void; showPin?: boolean; sortable?: Sortable }
export function AgentListRow({ claw, isSelected, messages, onClick, onTogglePin, showPin = true, sortable }: Props) {
  const latest = messages[messages.length - 1]
  return <div className="group relative"><div ref={sortable?.setNodeRef} style={sortable ? { transform: CSS.Transform.toString(sortable.transform), transition: sortable.transition, opacity: sortable.isDragging ? .4 : 1 } : undefined} {...sortable?.attributes} {...sortable?.listeners} role="button" tabIndex={0} onClick={onClick} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onClick() } }} className={cn("cursor-pointer border-l-[3px] px-3 py-2.5 hover:bg-accent/50", COLOR_CLASSES[claw.color]?.border || "border-border", isSelected && "bg-accent")}><div className="flex gap-2"><StatusDot status={claw.status} isStreaming={claw.isStreaming} /><div className="min-w-0 flex-1"><div className="flex items-center gap-2"><ClawTitle name={claw.name} githubIssueId={claw.githubIssueId} githubIssueUrl={undefined} className="min-w-0 flex-1 truncate font-mono text-xs font-semibold" />{claw.unreadCount > 0 && <span className="rounded-full bg-blue-600 px-1.5 text-[10px] font-bold text-white">{claw.unreadCount > 99 ? "99+" : claw.unreadCount}</span>}<time suppressHydrationWarning className="whitespace-nowrap text-[10px] text-muted-foreground">{formatRelativeTime(latest?.timestamp)}</time></div><p className="truncate text-xs text-muted-foreground">{claw.status === "error" && <span className="mr-1 rounded bg-red-500/15 px-1 text-[10px] text-red-400">Stopped</span>}{claw.status === "error" ? (claw.reason ? "Open for details" : "No details") : lastSnippet(messages)}</p>{claw.status === "provisioning" ? <BootstrapProgress claw={claw} variant="sidebar" /> : <div className={cn("mt-1 hidden items-center gap-1 overflow-hidden text-[10px] text-muted-foreground", isSelected ? "flex" : "group-hover:flex")}><span className="truncate">{claw.template}</span><span>·</span><span className="whitespace-nowrap">{formatUptime(claw.uptime)}</span>{claw.tags.slice(0, 2).map((tag) => <span key={tag} className="truncate">· {tag}</span>)}{claw.tags.length > 2 && <span>+{claw.tags.length - 2}</span>}</div>}{claw.contextUsage > 0 && <div className={cn("mt-1 h-0.5 overflow-hidden rounded bg-muted", !isSelected && "hidden group-hover:block")}><div className="h-full bg-green-500" style={{ width: `${claw.contextUsage}%` }} /></div>}</div></div></div>{showPin && <button type="button" onClick={onTogglePin} aria-label={claw.pinned ? "Unpin agent" : "Pin agent"} className={cn("absolute right-2 top-2 rounded p-1 opacity-0 hover:bg-background group-hover:opacity-100", claw.pinned && "opacity-100")}><Pin className={cn("size-3.5", claw.pinned && "fill-current")} /></button>}</div>
}
export function SortableAgentListRow(props: Omit<Props, "sortable">) { const sortable = useSortable({ id: props.claw.id }); return <AgentListRow {...props} sortable={sortable} /> }
