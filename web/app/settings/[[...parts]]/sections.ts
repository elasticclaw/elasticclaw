import { Cpu, Github, GitBranch, Key, LayoutTemplate, Lock, Shield, Sparkles, Stethoscope, Wrench, Zap, type LucideIcon } from "lucide-react"

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

export function isValidSection(s: string): s is Section {
  return VALID_SECTIONS.includes(s as Section)
}

// Sections scoped to a workspace: their canonical URL shape is
// /settings/{workspace}/{section} (the normalisation effect in
// settings-content.tsx rewrites /settings/{section} into it).
export const WORKSPACE_SECTIONS = new Set<Section>([
  "workspaces",
  "workflows",
  "github",
  "issue-trackers",
  "secrets",
  "mcp-servers",
])

// Single source of truth for the settings section catalogue: the app rail
// (components/nav-rail.tsx) and the settings screen both render from this
// list, so labels, icons and grouping cannot drift between them.
export interface SettingsNavItem {
  id: Section
  label: string
  icon: LucideIcon
}

export const SETTINGS_NAV_GROUPS: { label: string; items: SettingsNavItem[] }[] = [
  {
    label: "Workspace",
    items: [
      { id: "workspaces", label: "Overview", icon: LayoutTemplate },
      { id: "workflows", label: "Workflows", icon: GitBranch },
      { id: "github", label: "GitHub Apps", icon: Github },
      { id: "issue-trackers", label: "Issue Trackers", icon: Zap },
      { id: "secrets", label: "Secrets", icon: Lock },
      { id: "mcp-servers", label: "MCP Servers", icon: Zap },
    ],
  },
  {
    label: "System",
    items: [
      { id: "runtimes", label: "Sandboxes", icon: Cpu },
      { id: "models", label: "Models", icon: Key },
      { id: "authentication", label: "Authentication", icon: Shield },
      { id: "ai-config", label: "Configure with AI", icon: Sparkles },
    ],
  },
  {
    label: "Diagnostics",
    items: [
      { id: "doctor", label: "Doctor", icon: Stethoscope },
      { id: "troubleshoot", label: "Troubleshoot", icon: Wrench },
    ],
  },
]

// Resolves which settings section a pathname belongs to, mirroring the URL
// parsing in settings-content.tsx so the rail highlights the right item for
// both shapes: /settings/{section} and /settings/{workspace}/{section}
// (including the "_workspace" static-export sentinel and the bare /settings
// default). Returns null outside /settings and for legacy analytics URLs,
// which redirect to /analytics.
export function activeSettingsSection(pathname: string): Section | null {
  const parts = pathname.split("/").filter(Boolean)
  if (parts[0] !== "settings") return null
  const first = parts[1] ?? ""
  const second = parts[2] ?? ""
  if (first === LEGACY_ANALYTICS_SECTION || second === LEGACY_ANALYTICS_SECTION) return null
  if (first === "") return "workspaces"
  if (isValidSection(first)) return first
  // Workspace-scoped shape (or the "_workspace" placeholder): the second part
  // is the section, defaulting to the workspace overview.
  return isValidSection(second) ? second : "workspaces"
}
