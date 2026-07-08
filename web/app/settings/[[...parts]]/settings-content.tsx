"use client"

import { useParams, usePathname, useRouter } from "next/navigation"
import React, { useEffect, useState } from "react"
import dynamic from "next/dynamic"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, LayoutTemplate, Lock, Sparkles, Stethoscope, Wrench, GitBranch, ChevronDown, BarChart3 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
import Link from "next/link"
import { VALID_SECTIONS, type Section } from "./sections"
import { fetchWorkspaces, type Workspace } from "@/lib/api"
import { useSettingsData } from "../sections/use-settings-data"

// Each section is loaded as its own chunk so the /settings bundle only pulls
// in the code for the tab that is actually open.
const RuntimesSection = dynamic(() => import("../sections/runtimes"))
const LLMSection = dynamic(() => import("../sections/models"))
const GitHubSection = dynamic(() => import("../sections/github"))
const AuthenticationSection = dynamic(() => import("../sections/authentication"))
const IntegrationsSection = dynamic(() => import("../sections/issue-trackers"))
const WorkspacesSection = dynamic(() => import("../sections/workspaces"))
const WorkflowsSection = dynamic(() => import("../sections/workflows"))
const SecretsSection = dynamic(() => import("../sections/secrets"))
const MCPServersSection = dynamic(() => import("../sections/mcp-servers"))
const AIConfigSection = dynamic(() => import("../sections/ai-config"))
const AnalyticsSection = dynamic(() => import("../sections/analytics"))
const DoctorSection = dynamic(() => import("../sections/doctor"))
const TroubleshootSection = dynamic(() => import("../sections/troubleshoot"))
const TaskRunAnalyticsView = dynamic(
  () => import("@/components/task-run-analytics-view").then(m => m.TaskRunAnalyticsView),
  { loading: () => <div className="flex-1 p-6 text-sm text-muted-foreground">Loading analytics...</div> }
)

function isValidSection(s: string): s is Section {
  return VALID_SECTIONS.includes(s as Section)
}

const WORKSPACE_SECTIONS = new Set<Section>([
  "workspaces",
  "workflows",
  "workspace-analytics",
  "github",
  "issue-trackers",
  "secrets",
  "mcp-servers",
])

// Sentinel value used in static-export paths to represent "select first workspace"
const WORKSPACE_PLACEHOLDER = "_workspace"

export default function SettingsSectionPage() {
  const params = useParams()
  const pathname = usePathname()
  const router = useRouter()
  const paramParts = Array.isArray(params.parts) ? params.parts : []
  const pathnameParts = pathname
    .split("/")
    .filter(Boolean)
    .slice(1)
  const parts = pathname.startsWith("/settings") ? pathnameParts : paramParts
  const firstPart = parts[0] ?? ""
  const secondPart = parts[1] ?? ""
  const firstPartIsSection = isValidSection(firstPart)
  const firstPartIsPlaceholder = firstPart === WORKSPACE_PLACEHOLDER
  const hasRouteWorkspace = firstPart !== "" && !firstPartIsSection && !firstPartIsPlaceholder
  const rawSection = (hasRouteWorkspace || firstPartIsPlaceholder) ? (secondPart || "workspaces") : (firstPart || "workspaces")
  const section: Section = isValidSection(rawSection) ? rawSection : "workspaces"
  const rawWorkspace = hasRouteWorkspace ? firstPart : ""
  const routeWorkspace = rawWorkspace ? decodeURIComponent(rawWorkspace) : ""
  const routeHasOverviewSlug = hasRouteWorkspace && secondPart === "workspaces"

  // Redirect unsupported paths to a safe fallback:
  //   - Invalid section names → /settings/workspaces
  //   - More than 2 path parts → /settings/workspaces
  //   - Placeholder without workspace loaded yet → handled by workspace selection effect
  useEffect(() => {
    if (parts.length > 2) {
      router.replace("/settings/workspaces")
      return
    }
    if (!isValidSection(rawSection)) {
      router.replace("/settings/workspaces")
    }
  }, [parts.length, rawSection, router])

  const { settings, saving, error, success, save } = useSettingsData()
  const [version, setVersion] = useState("")
  const [hubPublicUrl, setHubPublicUrl] = useState("")
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [selectedWorkspace, setSelectedWorkspace] = useState("")
  const selectedWorkspaceLabel = selectedWorkspace || "No workspaces"
  const selectedWorkspaceInitial = selectedWorkspace ? selectedWorkspace.trim()[0].toUpperCase() : "-"

  useEffect(() => {
    const hubUrl = getHubUrl()
    const token = getAuthToken() || ""
    fetch(`${hubUrl}/api/hub-config`, { headers: { Authorization: `Bearer ${token}` } })
      .then((r) => r.json())
      .then((d) => { setVersion(d.version || "unknown"); setHubPublicUrl(d.hubUrl || "") })
      .catch(() => {})
  }, [])

  useEffect(() => {
    fetchWorkspaces()
      .then((data) => {
        setWorkspaces(data)
        setSelectedWorkspace((current) => {
          if (data.length === 0) return ""
          if (routeWorkspace && data.some((workspace) => workspace.name === routeWorkspace)) return routeWorkspace
          return data.some((workspace) => workspace.name === current) ? current : data[0].name
        })
      })
      .catch(() => {
        setWorkspaces([])
        setSelectedWorkspace("")
      })
  }, [routeWorkspace])

  // When the "_workspace" placeholder is used (static-export sentinel) or no
  // workspace is present in the URL, redirect to the actual first workspace.
  useEffect(() => {
    if (workspaces.length === 0 || !WORKSPACE_SECTIONS.has(section)) return
    const workspace = routeWorkspace && workspaces.some((item) => item.name === routeWorkspace)
      ? routeWorkspace
      : selectedWorkspace || workspaces[0].name
    const workspaceBase = `/settings/${encodeURIComponent(workspace)}`
    const target = section === "workspaces" ? workspaceBase : `${workspaceBase}/${section}`
    const needsRedirect = (!routeWorkspace && selectedWorkspace) || routeHasOverviewSlug || firstPartIsPlaceholder
    if (needsRedirect) {
      router.replace(target)
    }
  }, [firstPartIsPlaceholder, routeHasOverviewSlug, routeWorkspace, router, section, selectedWorkspace, workspaces])

  const navGroups: { label: string; items: { id: Section; label: string; icon: React.ElementType }[] }[] = [
    {
      label: "Workspace",
      items: [
        { id: "workspaces", label: "Overview", icon: LayoutTemplate },
        { id: "workflows", label: "Workflows", icon: GitBranch },
        { id: "workspace-analytics", label: "Analytics", icon: BarChart3 },
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
  const settingsHref = (id: Section) => {
    if (WORKSPACE_SECTIONS.has(id) && selectedWorkspace) {
      const workspaceBase = `/settings/${encodeURIComponent(selectedWorkspace)}`
      return id === "workspaces" ? workspaceBase : `${workspaceBase}/${id}`
    }
    return `/settings/${id}`
  }
  const selectWorkspace = (workspace: string) => {
    setSelectedWorkspace(workspace)
    const targetSection = WORKSPACE_SECTIONS.has(section) ? section : "workspaces"
    const workspaceBase = `/settings/${encodeURIComponent(workspace)}`
    router.push(targetSection === "workspaces" ? workspaceBase : `${workspaceBase}/${targetSection}`)
  }

  return (
    <div className="h-screen bg-background flex flex-col overflow-hidden">
      {/* Header */}
      <header className="border-b border-border px-6 py-4 flex items-center gap-4">
        <Button variant="ghost" size="icon" onClick={() => window.location.href = "/"}>
          <ChevronLeft className="size-4" />
        </Button>
        <h1 className="text-lg font-semibold">Configure</h1>
        {version && <span className="ml-auto text-xs text-muted-foreground font-mono">{version}</span>}
      </header>

      <div className="flex flex-1 min-h-0 overflow-hidden">
        {/* Left nav */}
        <aside className="w-56 border-r border-border p-4 flex flex-col overflow-y-auto">
          <div className="relative mb-1">
            <div className="pointer-events-none absolute left-3 top-1/2 z-10 flex size-5 -translate-y-1/2 items-center justify-center rounded bg-blue-600 text-[11px] font-semibold text-white shadow-sm">
              {selectedWorkspaceInitial}
            </div>
            <select
              aria-label="Workspace"
              value={selectedWorkspace}
              onChange={(event) => selectWorkspace(event.target.value)}
              disabled={workspaces.length === 0}
              className="h-10 w-full appearance-none rounded-lg border border-transparent bg-transparent pl-10 pr-10 text-sm font-semibold outline-none transition-colors hover:bg-secondary focus:bg-secondary"
            >
              {workspaces.length === 0 ? (
                <option value="">{selectedWorkspaceLabel}</option>
              ) : (
                workspaces.map((workspace) => (
                  <option key={workspace.name} value={workspace.name}>{workspace.name}</option>
                ))
              )}
            </select>
            <ChevronDown className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          </div>
          <div className="space-y-1 flex-1">
            {navGroups.map((group, groupIdx) => (
              <div key={group.label}>
                {groupIdx > 0 && <div className="h-5" />}
                {groupIdx > 0 && (
                  <p className="px-3 pb-1 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground/70">
                    {group.label}
                  </p>
                )}
                {group.items.map(({ id, label, icon: Icon }) => (
                  <Link
                    key={id}
                    href={settingsHref(id)}
                    className={cn(
                      "w-full flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors text-left",
                      section === id
                        ? "bg-primary/10 text-primary font-medium"
                        : "text-muted-foreground hover:bg-secondary hover:text-foreground"
                    )}
                  >
                    <Icon className="size-4 flex-shrink-0" />
                    {label}
                  </Link>
                ))}
              </div>
            ))}
          </div>
          {version && (
            <p className="text-xs text-muted-foreground/50 px-3 pt-4 font-mono">
              v{version}
            </p>
          )}
        </aside>

        {/* Content */}
        <main className={(section === "ai-config" || section === "troubleshoot" || section === "workspace-analytics") ? "flex-1 min-h-0 flex flex-col overflow-hidden" : "flex-1 overflow-y-auto p-8 max-w-2xl"}>
          {error && <p className="mb-4 text-sm text-red-500">{error}</p>}
          {success && <p className="mb-4 text-sm text-green-500">{success}</p>}

          {settings && section === "runtimes" && (
            <RuntimesSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "models" && (
            <LLMSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "github" && (
            <GitHubSection settings={settings} onSave={save} saving={saving} workspace={selectedWorkspace} />
          )}
          {settings && section === "authentication" && (
            <AuthenticationSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "issue-trackers" && (
            <IntegrationsSection settings={settings} onSave={save} saving={saving} selectedWorkspace={selectedWorkspace} hubPublicUrl={hubPublicUrl} />
          )}
          {section === "workspaces" && (
            <WorkspacesSection selectedWorkspace={selectedWorkspace} />
          )}
          {section === "workflows" && (
            <WorkflowsSection selectedWorkspace={selectedWorkspace} />
          )}
          {section === "workspace-analytics" && (
            <TaskRunAnalyticsView workspaceScope={selectedWorkspace} />
          )}
          {section === "secrets" && (
            <SecretsSection settings={settings} workspace={selectedWorkspace} />
          )}
          {settings && section === "mcp-servers" && (
            <MCPServersSection settings={settings} onSave={save} saving={saving} />
          )}
          {section === "ai-config" && (
            <AIConfigSection />
          )}
          {section === "analytics" && (
            <AnalyticsSection />
          )}
          {section === "doctor" && (
            <DoctorSection />
          )}
          {section === "troubleshoot" && (
            <TroubleshootSection />
          )}
        </main>
      </div>
    </div>
  )
}
