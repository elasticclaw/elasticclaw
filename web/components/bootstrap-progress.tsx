"use client"

import { CheckCircle2, Circle, Loader2 } from "lucide-react"
import type { Claw } from "@/lib/types"
import { cn } from "@/lib/utils"

const STEPS = [
  { key: "sandbox", label: "Sandbox" },
  { key: "runtime", label: "Runtime" },
  { key: "openclaw", label: "OpenClaw" },
  { key: "workspace", label: "Workspace" },
  { key: "connect", label: "Connect" },
] as const

type StepKey = typeof STEPS[number]["key"]

function activeStep(status?: string): StepKey {
  const value = (status || "").toLowerCase()
  if (value.includes("restoring checkpoint")) return "workspace"
  if (value.includes("connecting to hub") || value.includes("starting elasticclaw connector")) return "connect"
  if (value.includes("repo") || value.includes("workspace") || value.includes("sync")) return "workspace"
  if (value.includes("configuring openclaw") || value.includes("gateway")) return "openclaw"
  if (value.includes("runtime") || value.includes("install")) return "runtime"
  return "sandbox"
}

function friendlyDetail(status?: string): string {
  const value = status || "Creating sandbox"
  switch (activeStep(value)) {
    case "sandbox":
      return "Creating sandbox"
    case "runtime":
      return "Preparing runtime"
    case "openclaw":
      return "Starting OpenClaw"
    case "workspace":
      if (value.toLowerCase().includes("restoring checkpoint")) return value
      if (value.toLowerCase().includes("sync")) return "Syncing repositories"
      if (value.toLowerCase().includes("access")) return "Preparing repository access"
      return "Preparing workspace"
    case "connect":
      return "Connecting to hub"
  }
}

export function BootstrapProgress({
  claw,
  variant = "compact",
}: {
  claw: Claw
  variant?: "sidebar" | "compact" | "full"
}) {
  if (claw.status !== "provisioning") return null

  const current = activeStep(claw.bootstrap_status)
  const currentIndex = STEPS.findIndex((step) => step.key === current)
  const detail = friendlyDetail(claw.bootstrap_status)

  if (variant === "sidebar") {
    return (
      <div className="mt-1.5 w-full min-w-0 overflow-hidden space-y-1">
        <div className="flex min-w-0 items-center justify-between gap-2 text-[9px]">
          <span className="min-w-0 truncate font-medium text-blue-400">{detail}</span>
          <span className="shrink-0 text-muted-foreground/50">{currentIndex + 1}/{STEPS.length}</span>
        </div>
        <div className="grid min-w-0 grid-cols-5 gap-0.5">
          {STEPS.map((step, index) => (
            <div
              key={step.key}
              className={cn(
                "h-0.5 rounded-full bg-border",
                index < currentIndex && "bg-blue-500/70",
                index === currentIndex && "bg-blue-400 animate-pulse"
              )}
              title={step.label}
            />
          ))}
        </div>
      </div>
    )
  }

  if (variant === "compact") {
    return (
      <div className="mt-2 space-y-1.5">
        <div className="flex items-center justify-between text-[10px]">
          <span className="font-medium text-blue-400">{detail}</span>
          <span className="text-muted-foreground/60">{currentIndex + 1}/{STEPS.length}</span>
        </div>
        <div className="grid grid-cols-5 gap-1">
          {STEPS.map((step, index) => (
            <div
              key={step.key}
              className={cn(
                "h-1 rounded-full bg-border",
                index < currentIndex && "bg-blue-500/70",
                index === currentIndex && "bg-blue-400 animate-pulse"
              )}
              title={step.label}
            />
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className="border-t border-border/60 px-6 py-3">
      <div className="mb-2 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <Loader2 className="size-3.5 animate-spin text-blue-400" />
          <span className="text-sm font-medium text-foreground">Setting up agent</span>
        </div>
        <span className="text-xs text-muted-foreground">{detail}</span>
      </div>
      <div className="grid grid-cols-5 gap-2">
        {STEPS.map((step, index) => {
          const isDone = index < currentIndex
          const isCurrent = index === currentIndex
          return (
            <div key={step.key} className="min-w-0">
              <div
                className={cn(
                  "mb-1 h-1 rounded-full bg-border",
                  isDone && "bg-blue-500/70",
                  isCurrent && "bg-blue-400 animate-pulse"
                )}
              />
              <div className="flex min-w-0 items-center gap-1.5">
                {isDone ? (
                  <CheckCircle2 className="size-3 shrink-0 text-blue-400" />
                ) : isCurrent ? (
                  <Loader2 className="size-3 shrink-0 animate-spin text-blue-400" />
                ) : (
                  <Circle className="size-3 shrink-0 text-muted-foreground/40" />
                )}
                <span className={cn(
                  "truncate text-[11px]",
                  isCurrent ? "font-medium text-foreground" : "text-muted-foreground"
                )}>
                  {step.label}
                </span>
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}
