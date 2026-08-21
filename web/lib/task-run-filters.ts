import type { TaskRunAnalyticsFilters, TaskRunAttempt, TaskRunEvent, TaskRunPR, TaskRunStage } from "@/lib/types"

export type DetailState = {
  attempts: TaskRunAttempt[]
  events: TaskRunEvent[]
  prs: TaskRunPR[]
  stages: TaskRunStage[]
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
