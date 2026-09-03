export type ClawStatus = "connected" | "idle" | "offline" | "provisioning" | "error"

export const INFRA_EVENT_TYPES = [
  "dependency_down",
  "dependency_degraded",
  "dependency_recovered",
  "provider_limit_opened",
  "provider_limit_exhausted",
  "provider_limit_released",
] as const

export type InfraEventType = (typeof INFRA_EVENT_TYPES)[number]

export interface InfraRoute {
  via: string
  events?: InfraEventType[]
}

export interface InfraNotificationsConfig {
  enabled?: boolean
  routes?: InfraRoute[]
  pollInterval?: string
  repeatAfter?: string
}

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
  openPrCount: number
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
  /** ISO instant when the model provider's allowance returns; absent = not limited. */
  llm_limited_until?: string
}

export interface Message {
  id: string
  role: "user" | "claw" | "system" | "hub" | "activity" | "activity_summary" | "state"
  content: string
  format?: string // "pre" = preserve whitespace
  timestamp: Date
  activity?: AgentActivity
  activitySummary?: ActivitySummary
  // API fields
  claw_id?: string
  tenant_id?: string
  userLogin?: string
  optimisticSelf?: boolean
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
  call_id?: string
  duration_ms?: number
  exit_code?: number
  result?: string
  subagent_name?: string
  subagent_type?: string
  subagent_model?: string
  subagent_prompt?: string
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
  open_pr_count?: number
  tags?: string[]
  color?: string
  ssh_host?: string
  ssh_port?: number
  ssh_user?: string
  bootstrap_status?: string
  github_issue_id?: string
  github_issue_url?: string
  llm_limited_until?: string
}

export interface ApiMessage {
  id: string
  claw_id: string
  tenant_id: string
  role: "user" | "claw" | "hub" | "activity" | "activity_summary" | "state"
  content: string
  format?: string
  created_at: string
  user_login?: string
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
  outcomesByDay: { date: string; clean: number; humanInTheLoop: number; warning: number; failed: number }[] | null
  funnel: { agentStarted: number; prOpened: number; prFinished: number }
  costPerMergedPr: { weekly: { weekStart: string; costUsd: number; mergedPrs: number; costPerMergedPr: number }[] | null; average: number }
  mergeRate: number
  successRate: number
  uniqueTickets: number
  ticketSuccessRate: number
  ticketsByDay: { date: string; delivered: number; inProgress: number; failed: number }[] | null
  runsPerTicket: { bucket: string; tickets: number }[] | null
  topTicketsByCost: { ticketKey: string; issueId: string; issueTitle: string; costUsd: number; runs: number; outcome: string }[] | null
  prior?: {
    successRate: number
    ticketSuccessRate: number
    uniqueTickets: number
    mergeRate: number
  }
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
  readyAt: number
  readyToMergeMs: number
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

export interface TaskRunStage {
  stageId: string
  label: string
  enteredAt: number
  exitedAt?: number
  durationMs: number
  source: string
}

export interface TaskRunsResponse {
  runs: TaskRunSummary[]
  nextCursor?: string
  limit: number
}

export interface AnalyticsTicketRunSummary {
  runId: string
  status: string
  phase: string
  model: string
  attemptCount: number
  cost: number
  totalTokens: number
  humanTouches: number
  startedAt: number
  lastActivity: number
}

export interface AnalyticsTicketPR extends TaskRunPR {
  runId: string
}

export interface AnalyticsTicketStoryEntry {
  id: string
  eventType: string
  label: string
  actor: string
  time: number
  runId: string
  kind: "good" | "bad" | "human" | "neutral"
  count: number
}

export interface AnalyticsTicket {
  ticketKey: string
  issueId: string
  issueTitle: string
  status: "delivered" | "pr_open" | "in_progress" | "failed"
  requester: string
  team?: string
  priority: string
  ask: string
  source: string
  repo?: string
  workflowName?: string
  workspaceName?: string
  reportedAt: number
  runs: AnalyticsTicketRunSummary[]
  runCount: number
  attemptCount: number
  failedRunCount: number
  cost: number
  totalTokens: number
  humanTouches: number
  prs: AnalyticsTicketPR[]
  mergedPrCount: number
  openPrCount: number
  timeToFirstRun: number
  leadTime: number
  lastActivity: number
  story: AnalyticsTicketStoryEntry[]
}

export interface AnalyticsTicketsResponse {
  tickets: AnalyticsTicket[]
  nextCursor?: string
  limit: number
  total: number
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
  spanId: string
  spanKind: string
  durationMs: number
  status: 'OK' | 'ERROR'
  records: TaskRunLogRecord[]
  createdAt: number
}

export interface TaskRunLogRecord {
  ts: number
  sev: 'TRACE' | 'DEBUG' | 'INFO' | 'WARN' | 'ERROR' | 'FATAL'
  severityNumber: number
  body: string
  attrs: Record<string, unknown>
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

export type DependencyStatusValue = "operational" | "degraded" | "downtime" | "limited" | "unknown"

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
  /** Set only when the downtime ends at a known time (a capped provider account). */
  regainAt?: string
  checkedAt: string
}

export interface DependencyStatusResponse {
  dependencies: DependencyStatus[]
  downtimeCount: number
  /** Accounts out of allowance — counted apart from outages, they are not the same problem. */
  limitedCount: number
  checkedAt: string
}

export interface WorkflowRun {
  id: string
  tenant_id?: string
  workflow_name: string
  workspace_name: string
  trigger_type: string
  status: string
  result?: string
  claw_id?: string
  run_context?: Record<string, unknown>
  started_at?: string
  finished_at?: string
  created_at: string
}

export interface WorkflowRunsResponse {
  runs: WorkflowRun[]
  count: number
}

export interface WorkflowV2RunAttempt {
  id: string
  run_id: string
  claw_id: string
  number: number
  status: string
  started_at: string
  heartbeat_at?: string
  finished_at?: string
}

export interface WorkflowV2Run {
  run_id: string
  attempt_id: string
  attempt_number: number
  tenant_id?: string
  workspace_name: string
  workflow_name: string
  state: string
  display_phase: string
  run_status: string
  attempt_status: string
  waiting_reason?: string
  trigger_type: string
  claw_id?: string
  created_at: string
  started_at: string
  updated_at: string
  finished_at?: string
  attempt_finished_at?: string
}

export interface WorkflowV2RunsResponse {
  runs: WorkflowV2Run[]
  count: number
}
