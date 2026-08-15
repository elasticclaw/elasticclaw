"use client"

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
        <div className="flex min-w-0 items-center justify-between gap-2 font-mono text-[9px] text-muted-foreground">
          <span className="min-w-0 truncate">{detail}</span>
          <span className="shrink-0">{currentIndex + 1}/{STEPS.length}</span>
        </div>
        <div className="grid min-w-0 grid-cols-5 gap-0.5">
          {STEPS.map((step, index) => (
            <div
              key={step.key}
              className={cn(
                "h-[3px] bg-foreground/14",
                index <= currentIndex && "bg-primary"
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
        <div className="flex items-center justify-between font-mono text-[10px] text-muted-foreground">
          <span>{detail}</span>
          <span>{currentIndex + 1}/{STEPS.length}</span>
        </div>
        <div className="grid grid-cols-5 gap-1">
          {STEPS.map((step, index) => (
            <div
              key={step.key}
              className={cn(
                "h-[3px] bg-foreground/14",
                index <= currentIndex && "bg-primary"
              )}
              title={step.label}
            />
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className="border-t border-border px-6 py-3">
      <div className="grid gap-1.5">
        <div className="grid grid-cols-5 gap-1">
          {STEPS.map((step, index) => (
            <div
              key={step.key}
              className={cn("h-[3px] bg-foreground/14", index <= currentIndex && "bg-primary")}
              title={step.label}
            />
          ))}
        </div>
        <span className="truncate font-mono text-[10px] text-muted-foreground">
          {detail} · {currentIndex + 1}/{STEPS.length}
        </span>
      </div>
    </div>
  )
}
