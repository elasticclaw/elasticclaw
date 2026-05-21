export const VALID_SECTIONS = [
  "runtimes",
  "models",
  "github",
  "authentication",
  "issue-trackers",
  "factories",
  "secrets",
  "templates",
  "ai-config",
  "mcp-servers",
  "webhooks",
  "doctor",
  "analytics",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]
