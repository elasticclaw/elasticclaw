"use client"

import { BarChart3, FileText, FlaskConical, Lightbulb } from "lucide-react"
import { cn } from "@/lib/utils"

export const chatModes = [
  { id: "explore_idea", name: "Explore idea", icon: Lightbulb, description: "Turn a user problem into feature ideas, grounded in what the data and docs already say.", example: "People keep asking coaches how to swap a meal" },
  { id: "validate_hypothesis", name: "Validate hypothesis", icon: FlaskConical, description: "Shape a testable hypothesis with a metric, today's baseline and a target.", example: "Skipping measurements will raise quiz completion" },
  { id: "ask_the_data", name: "Ask the data", icon: BarChart3, description: "Ask a plain question, get numbers and where they came from.", example: "How did D7 retention move after the Sep release?" },
  { id: "write_brief", name: "Write brief", icon: FileText, description: "Turn a conversation into a brief, then into a ticket or a page.", example: "Write the brief for the meal-swap idea" },
] as const

export type ChatMode = typeof chatModes[number]["id"]

export function ModeCards({ mode, onSelect }: { mode: ChatMode | null; onSelect: (mode: ChatMode) => void }) {
  return (
    <div className="grid grid-cols-4 gap-2.5">
      {chatModes.map(({ id, name, icon: Icon, description, example }) => (
        <button key={id} type="button" aria-pressed={mode === id} onClick={() => onSelect(id)} className={cn("rounded-xl border bg-card p-4 text-left transition-colors hover:border-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring", mode === id && "border-primary bg-accent")}>
          <span className="mb-2 inline-flex size-7 items-center justify-center rounded-lg bg-muted"><Icon className="size-4" /></span>
          <h2 className="text-sm font-semibold">{name}</h2>
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{description}</p>
          <p className="mt-2 text-xs italic leading-relaxed text-muted-foreground">“{example}”</p>
        </button>
      ))}
    </div>
  )
}
