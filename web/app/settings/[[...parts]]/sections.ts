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
  "analytics",
  "doctor",
  "troubleshoot",
] as const

export type Section = (typeof VALID_SECTIONS)[number]

// Former settings section promoted to the top-level /analytics route. It is
// not a valid section anymore, but its URL shapes must keep working: the
// static export still pre-renders them (see page.tsx) and settings-content.tsx
// redirects them to /analytics with the query string preserved.
export const LEGACY_ANALYTICS_SECTION = "workspace-analytics"
