import type { TaskRunAnalyticsFilters, TaskRunAttempt, TaskRunEvent, TaskRunPR } from "@/lib/types"

export type DetailState = {
  attempts: TaskRunAttempt[]
  events: TaskRunEvent[]
  prs: TaskRunPR[]
}

export const urlFilterKeys = [
  "status",
  "factory",
  "workflow",
  "repo",
  "model",
  "warningType",
  "failureType",
] as const satisfies readonly (keyof TaskRunAnalyticsFilters)[]
