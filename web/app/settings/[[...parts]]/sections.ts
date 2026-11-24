export const VALID_SECTIONS = [
  "runtimes",
  "models",
  "github",
  "authentication",
  "api-tokens",
  "issue-trackers",
  "workspaces",
  "workflows",
  "secrets",
  "ai-config",
  "mcp-servers",
  "notifier",
  "beta-features",
  "analytics",
  "doctor",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]
