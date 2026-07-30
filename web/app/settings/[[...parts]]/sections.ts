export const VALID_SECTIONS = [
  "runtimes",
  "models",
  "github",
  "authentication",
  "issue-trackers",
  "workspaces",
  "workflows",
  "routines",
  "workspace-analytics",
  "secrets",
  "ai-config",
  "mcp-servers",
  "analytics",
  "doctor",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]
