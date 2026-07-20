export type ClawStatus = "connected" | "idle" | "offline" | "provisioning" | "error"

export interface Claw {
  id: string
  name: string
  template: string
  status: ClawStatus
  uptime: number // in seconds, computed from created_at
  unreadCount: number
  isStreaming: boolean
  pinned: boolean
  tags: string[]
  color: string // accent color name, e.g. "blue", "emerald"
  contextUsage: number // 0-100 percentage, hardcoded 0 for now
  description?: string
  reason?: string // stop reason when status is error
  bootstrap_status?: string
  githubIssueId?: string
  githubIssueUrl?: string
  // SSH / terminal access
  ssh_host?: string
  ssh_port?: number
  ssh_user?: string
  // API fields
  last_seen?: string
  created_at?: string
  tenant_id?: string
}

export interface Message {
  id: string
  role: "user" | "claw" | "system" | "hub" | "activity" | "activity_summary"
  content: string
  format?: string // "pre" = preserve whitespace
  timestamp: Date
  activity?: AgentActivity
  activitySummary?: ActivitySummary
  // API fields
  claw_id?: string
  tenant_id?: string
}

export interface ActivitySummary {
  count: number
  from?: string
  to?: string
}

export interface AgentActivity {
  kind: string
  stream?: string
  phase?: string
  tool?: string
  detail?: string
  command?: string
  path?: string
  url?: string
  message?: string
  error?: string
}

// Raw API types
export interface ApiClaw {
  id: string
  name: string
  template: string
  status: "connected" | "offline" | "provisioning" | "starting" | "error" | "idle"
  last_seen: string
  created_at: string
  tenant_id: string
  context_usage?: number
  tags?: string[]
  color?: string
  ssh_host?: string
  ssh_port?: number
  ssh_user?: string
  bootstrap_status?: string
  github_issue_id?: string
  github_issue_url?: string
}

export interface ApiMessage {
  id: string
  claw_id: string
  tenant_id: string
  role: "user" | "claw" | "hub" | "activity" | "activity_summary"
  content: string
  format?: string
  created_at: string
}

export interface CreateClawRequest {
  name: string
  template: string
  provider: string
  default_model?: string
  files?: string[]
}

export interface TaskRunAnalyticsFilters {
  status?: string
  ownerType?: string
  workspace?: string
  workflow?: string
  factory?: string
  integration?: string
  repo?: string
  model?: string
  warningType?: string
  failureType?: string
  humanTouched?: boolean
  mergedPrs?: boolean
  analyticsEnabled?: boolean
  requiresPr?: boolean
  from?: string
  to?: string
  limit?: number
  cursor?: string
}

export interface TaskRunAnalyticsSummary {
  totalRuns: number
  byStatus: Record<string, number>
  warningBreakdown: Record<string, number>
  failureBreakdown: Record<string, number>
  humanInteractions: number
  prCounts: {
    total: number
    open: number
    merged: number
    closed: number
  }
  prior?: TaskRunAnalyticsSummary
}

export interface CostBucket {
  costUsd: number
}

export interface CostToday {
  costUsd: number
  totalTokens: number
  deltaPctVsYesterday: number | null
}

export interface CostProjection {
  costUsd: number
  confidence: "high" | "medium" | "low"
  basis: string
}

export interface CostDailyPoint {
  date: string
  costUsd: number
  totalTokens: number
  runCount: number
}

export interface CostModelSeries { model: string; dailySeries: CostDailyPoint[] }

export interface CostOverview {
  today: CostToday
  week: CostBucket
  month: CostBucket
  projectedMonth: CostProjection | null
  dailySeries: CostDailyPoint[]
  prior: { periodCostUsd: number }
  priorPeriodCostUsd?: number
  seriesByModel?: CostModelSeries[]
}

export interface GeneralStatMetric {
  avgMs: number | null
  samples: number
  authoritativeSamples?: number
}

export interface GeneralStats {
  ticketToPrMs: GeneralStatMetric
  prOpenToMergeMs: GeneralStatMetric
  aiImplMs: GeneralStatMetric
  prior?: GeneralStats
}

export interface AnalyticsEffectiveness {
  outcomesByDay: { date: string; clean: number; warning: number; failed: number }[]
  funnel: { started: number; agentFinished: number; prOpened: number; prMerged: number }
  costPerMergedPr: { weekly: { weekStart: string; costUsd: number; mergedPrs: number; costPerMergedPr: number }[]; average: number }
  mergeRate: number
  successRate: number
}

export interface AnalyticsCostDriver {
  name: string
  runs: number
  successRate: number
  costUsd: number
  mergedPrs: number
  costPerMergedPr: number
  dailyCost: CostDailyPoint[]
}

export interface TaskRunSummary {
  runId: string
  initialAttemptId: string
  currentAttemptId: string
  status: string
  phase: string
  attemptCount: number
  ownerType: string
  workspaceName: string
  workflowName: string
  factoryName: string
  ownerId: string
  ownerDisplayName: string
  runKind: string
  integration: string
  integrationWorkspace: string
  issueId: string | null
  clawId: string
  model: string | null
  llmKey: string
  repo: string | null
  primaryPrUrl: string | null
  prCount: number
  openPrCount: number
  mergedPrCount: number
  closedPrCount: number
  warningTypes: string[]
  failureType: string | null
  humanInteractionCount: number
  startedAt: number
  queuedAt: number
  provisionStartedAt: number
  agentStartedAt: number
  prOpenedAt: number
  mergedAt: number
  finishedAt: number
  timeoutAt: number
  lastEventAt: number
  materializedAt: number
  updatedAt: number
  analyticsEnabled: boolean
  requiresPr: boolean
  excludedReason: string | null
  inputTokens?: number
  outputTokens?: number
  totalTokens?: number
  estimatedCostUsd?: number
  issueTitle?: string
}

export interface TaskRunsResponse {
  runs: TaskRunSummary[]
  nextCursor?: string
  limit: number
}

export interface TaskRunAttempt {
  id: string
  attemptId: string
  attemptNumber: number
  triggerId: string
  clawId: string
  status: string
  failureType: string | null
  startedAt: number
  finishedAt: number
  createdAt: number
  updatedAt: number
}

export interface TaskRunEvent {
  id: string
  attemptId: string
  eventKey: string
  source: string
  sourceEventId: string
  sourceDeliveryId: string
  eventType: string
  eventTime: number
  observedAt: number
  actorType: string
  actorId: string
  actorLogin: string
  actorDisplayName: string
  interactionRole: string
  targetType: string
  targetId: string
  targetUrl: string
  warningType: string
  failureType: string
  detail: Record<string, unknown>
  createdAt: number
}

export interface TaskRunOutput {
  clawId: string
  attemptId?: string
  stageId: string
  outputName: string
  stdout: string
  stderr: string
  exitCode: number
  createdAt: number
}

export interface TaskRunPR {
  id: string
  repo: string
  prNumber: number
  url: string
  headSha: string
  headBranch: string
  lastAgentHeadSha: string
  baseBranch: string
  state: string
  merged: boolean
  openedAt: number
  closedAt: number
  mergedAt: number
  mergedByLogin: string
  createdAt: number
  updatedAt: number
}

export interface TaskRunFilterOptions {
  workspaces: string[]
  workflows: string[]
  factories: string[]
  integrations: string[]
  repos: string[]
  models: string[]
  statuses: string[]
  warningTypes: string[]
  failureTypes: string[]
}

export type DependencyStatusValue = "operational" | "degraded" | "downtime" | "unknown"

export enum DependencyKind {
  Model = "model",
  Sandbox = "sandbox",
  IssueTracker = "issue_tracker",
}

export interface DependencyStatus {
  id: string
  name: string
  kind: DependencyKind
  status: DependencyStatusValue
  message?: string
  source?: string
  checkedAt: string
}

export interface DependencyStatusResponse {
  dependencies: DependencyStatus[]
  downtimeCount: number
  checkedAt: string
}
