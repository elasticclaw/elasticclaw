export const VALID_SECTIONS = [
  "runtimes",
  "models",
  "github",
  "authentication",
  "issue-trackers",
  "workspaces",
  "secrets",
  "ai-config",
  "mcp-servers",
  "webhooks",
  "doctor",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]
