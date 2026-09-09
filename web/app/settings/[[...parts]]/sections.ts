export const VALID_SECTIONS = [
  "runtimes",
  "models",
  "github",
  "authentication",
  "issue-trackers",
  "workspaces",
  "workflows",
  "secrets",
  "ai-config",
  "mcp-servers",
  "notifier",
  "analytics",
  "doctor",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]
