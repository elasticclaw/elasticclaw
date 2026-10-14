import { CircleCheck, FileCode2, Info, Ticket, TriangleAlert } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatAction, AIChatBlock, AIChatAnswersSummary } from "@/lib/api"

// Offsets are Unicode code points, matching the runner's rune count.
export function splitContent(content: string, blocks: AIChatBlock[] = []): (string | AIChatBlock)[] {
  const points = Array.from(content)
  const parts: (string | AIChatBlock)[] = []
  let offset = 0
  for (const block of blocks) {
    if (!["stat_tiles", "callout", "table", "actions"].includes(block.type)) continue
    const at = Math.max(offset, Math.min(points.length, block.at))
    if (at > offset) parts.push(points.slice(offset, at).join(""))
    parts.push(block)
    offset = at
  }
  if (offset < points.length) parts.push(points.slice(offset).join(""))
  return parts
}

export function InlineBlock({ block, onAction, ticketCaption }: { block: AIChatBlock; onAction?: (action: AIChatAction) => void; ticketCaption?: string }) {
  switch (block.type) {
    case "stat_tiles": {
      const columns = ["", "sm:grid-cols-1", "sm:grid-cols-2", "sm:grid-cols-3", "sm:grid-cols-2 lg:grid-cols-4"][block.data.tiles.length]
      return <div className={`my-3 grid grid-cols-1 gap-2.5 ${columns}`}>
        {block.data.tiles.map((tile, index) => <div key={index} className="min-w-0 rounded-xl border bg-card px-3.5 py-3 leading-snug">
          <div className="text-[11.5px] text-muted-foreground">{tile.label}</div>
          <div className="my-1.5 text-[22px] leading-tight font-bold tracking-tight">{tile.value}</div>
          {tile.caption && <div className="text-[11.5px] text-muted-foreground">{tile.caption}</div>}
        </div>)}
      </div>
    }
    case "callout": {
      const Icon = { info: Info, warning: TriangleAlert, success: CircleCheck }[block.data.tone]
      return <div className="my-3 flex gap-3 rounded-xl border bg-card p-4">
        <Icon className={`mt-0.5 size-4 shrink-0 ${block.data.tone === "warning" ? "text-amber-500" : block.data.tone === "success" ? "text-green-500" : "text-muted-foreground"}`} />
        <div>{block.data.title && <div className="font-semibold">{block.data.title}</div>}<div className="whitespace-pre-wrap">{block.data.text}</div></div>
      </div>
    }
    case "table": return <div className="my-3 overflow-x-auto rounded-xl border">
      <table className="w-full text-left text-sm"><thead className="bg-muted/50"><tr>{block.data.columns.map((column, index) => <th key={index} scope="col" className="border-b px-3 py-2 font-medium">{column}</th>)}</tr></thead>
        <tbody>{block.data.rows.map((row, index) => <tr key={index} className="border-b last:border-0">{row.map((cell, col) => <td key={col} className="px-3 py-2 align-top">{cell}</td>)}</tr>)}</tbody>
      </table>
    </div>
    case "actions":
      if (!onAction) return null
      return <div className="my-5 space-y-2.5"><div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Wrap up</div>
        <div className="grid gap-2.5 sm:grid-cols-2">{block.actions.map((action) => {
          const Icon = action === "artifact" ? FileCode2 : Ticket
          return <Button key={action} variant="outline" className="h-auto justify-start gap-3 rounded-xl p-3 text-left" onClick={() => onAction(action)}>
            <Icon className="size-4 shrink-0" /><span><span className="block font-semibold">{action === "artifact" ? "Create HTML artifact" : "Open Linear ticket"}</span>
              <span className="block text-xs font-normal text-muted-foreground">{action === "artifact" ? "A page served by the factory" : ticketCaption || "Linear"}</span></span>
          </Button>
        })}</div>
      </div>
    default: return null
  }
}

export function AnswersSummary({ summary }: { summary: AIChatAnswersSummary }) {
  const shorten = (text: string) => Array.from(text).length > 60 ? Array.from(text).slice(0, 60).join("") + "…" : text
  return <div className="rounded-xl border bg-card px-3.5 py-3 text-[13px] leading-snug">
    <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">Your answers</div>
    <div>{summary.answers.map((item, index) => <span key={index}>{index > 0 && <span className="text-muted-foreground"> · </span>}<strong className="font-semibold">{shorten(item.question)}:</strong> <span className="text-muted-foreground">{item.answer}</span></span>)}
      {summary.skipped > 0 && <span className="text-muted-foreground">{summary.answers.length > 0 && " · "}{summary.skipped} {summary.skipped === 1 ? "step" : "steps"} skipped</span>}
    </div>
  </div>
}
