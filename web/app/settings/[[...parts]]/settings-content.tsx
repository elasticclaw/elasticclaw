"use client"

import { useParams, usePathname, useRouter } from "next/navigation"
import React, { useEffect, useState, useCallback, useRef } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Copy, Check, LayoutTemplate, Trash2, Lock, Sparkles, Send, RotateCcw, Eye, EyeOff, ExternalLink, AlertTriangle, X, CheckCircle2, Webhook, Stethoscope, ArrowRight, Wrench, GitBranch, ChevronDown, BarChart3 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import Link from "next/link"
import { VALID_SECTIONS, type Section } from "./sections"
import { fetchWorkspaces, updateWorkflowControls, type RepositoryAccess, type Workspace, type Workflow } from "@/lib/api"

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

interface LLMKeyView {
  name: string
  provider: string
  keySet: boolean
  default: boolean
  defaultModel?: string
}

interface GitHubAppPermission {
  name: string
  granted: string
  needed: string
  ok: boolean
}

interface GitHubAppView {
  appId: number
  url?: string
  keySet: boolean
  permissions?: GitHubAppPermission[]
  permCheckOk?: boolean
  permCheckError?: string
}

interface WorkspaceGitHubAppView {
  name: string
  appId: number
  url?: string
  installation?: string
  installations?: string[]
  private_key_set?: boolean
  privateKeySet?: boolean
}

interface SettingsData {
  llmKeys: LLMKeyView[]
  providers: Record<string, {
    type: string
    enabled: boolean
    apiUrl?: string
    apiKeySet?: boolean
    defaultSnapshot?: string
    tokenSet?: boolean
    defaultTtl?: string
    defaultInstanceType?: string
    defaultCpu?: number
    defaultMemory?: string
    defaultDisk?: string
    sshKeySet?: boolean
    sshPublicKey?: string
  }>
  github: GitHubAppView[]
  sshPublicKeys: string[]
  integrations?: {
    linear?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    shortcut?: Array<{ workspace: string; tokenSet: boolean }>
    githubIssues?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
  }
  secrets?: string[]
  mcpServers?: Array<{
    name: string
    source: string
    package?: string
    image?: string
    url?: string
    enabled: boolean
    config?: Record<string, string>
    secrets?: string[]
    command?: string[]
  }>
  auth?: {
    githubOAuth?: {
      clientId: string
      clientSecretSet: boolean
      allowedUsers: string[]
      allowedOrgs: string[]
      allowedTeams: string[]
    }
    access?: {
      admins: string[]
      viewRequiresTags: string[]
      interactRequiresTags: string[]
    }
    disablePasswordAuth?: boolean
  }
  concurrencyGroups?: ConcurrencyGroup[]
  maxConcurrentClaws?: number
}

interface ConcurrencyGroup {
  name: string
  limit: number
}

async function fetchSettings(): Promise<SettingsData> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error("Failed to load settings")
  return res.json()
}

async function patchSettings(patch: object): Promise<void> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    method: "PATCH",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  })
  if (!res.ok) throw new Error(await res.text())
}

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
    .map((part) => decodeURIComponent(part))
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

  const [settings, setSettings] = useState<SettingsData | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  const [success, setSuccess] = useState("")
  const [version, setVersion] = useState("")
  const [hubPublicUrl, setHubPublicUrl] = useState("")
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [selectedWorkspace, setSelectedWorkspace] = useState("")
  const selectedWorkspaceLabel = selectedWorkspace || "No workspaces"
  const selectedWorkspaceInitial = selectedWorkspace ? selectedWorkspace.trim()[0].toUpperCase() : "-"

  const load = useCallback(async () => {
    try {
      setSettings(await fetchSettings())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load")
    }
  }, [])

  useEffect(() => { load() }, [load])

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

  async function save(patch: object): Promise<boolean> {
    setSaving(true)
    setError("")
    setSuccess("")
    try {
      await patchSettings(patch)
      setSuccess("Saved")
      await load()
      setTimeout(() => setSuccess(""), 2000)
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed")
      return false
    } finally {
      setSaving(false)
    }
  }

  // Silent save: patches without the global 'Saved' banner (used for toggle-style updates)
  async function saveSilent(patch: object) {
    try {
      await patchSettings(patch)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed")
    }
  }

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
        { id: "analytics", label: "Analytics", icon: BarChart3 },
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
        <main className={(section === "ai-config" || section === "troubleshoot") ? "flex-1 min-h-0 flex flex-col overflow-hidden" : "flex-1 overflow-y-auto p-8 max-w-2xl"}>
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
            <AnalyticsSection selectedWorkspace={selectedWorkspace} />
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

const SANDBOX_PROVIDER_OPTIONS = [
  { value: "replicated", label: "Replicated CMX", description: "Kubernetes-based VM provider" },
  { value: "daytona", label: "Daytona", description: "Development environment provider" },
  { value: "exedev", label: "exe.dev", description: "Persistent VM provider with SSH access" },
]

interface SandboxProviderView {
  name: string
  type: string
  label: string
  description: string
  configured: boolean
  apiUrl?: string
  apiKeySet?: boolean
  defaultSnapshot?: string
  tokenSet?: boolean
  defaultTtl?: string
  defaultInstanceType?: string
}

function RuntimesSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const [newKey, setNewKey] = useState("")
  const providers = settings.providers || {}

  const configuredProviders: SandboxProviderView[] = Object.entries(providers)
    .filter(([_, p]) => p != null)
    .map(([name, p]) => {
      const opt = SANDBOX_PROVIDER_OPTIONS.find(o => o.value === name)
      return {
        name,
        type: p.type || name,
        label: opt?.label || name,
        description: opt?.description || "",
        configured: !!(p.tokenSet || p.apiKeySet),
        apiUrl: p.apiUrl,
        apiKeySet: p.apiKeySet,
        defaultSnapshot: p.defaultSnapshot,
        tokenSet: p.tokenSet,
        defaultTtl: p.defaultTtl,
        defaultInstanceType: p.defaultInstanceType,
        defaultCpu: p.defaultCpu,
        defaultMemory: p.defaultMemory,
        defaultDisk: p.defaultDisk,
      }
    })

  // Modal state
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [editName, setEditName] = useState<string | null>(null)
  const [copiedKey, setCopiedKey] = useState(false)

  // Form state
  const [formProvider, setFormProvider] = useState("replicated")
  const [formToken, setFormToken] = useState("")
  const [formApiUrl, setFormApiUrl] = useState("")
  const [formApiKey, setFormApiKey] = useState("")
  const [formDefaultTtl, setFormDefaultTtl] = useState("")
  const [formDefaultInstanceType, setFormDefaultInstanceType] = useState("")
  const [formDefaultSnapshot, setFormDefaultSnapshot] = useState("")
  const [formDefaultCpu, setFormDefaultCpu] = useState("")
  const [formDefaultMemory, setFormDefaultMemory] = useState("")
  const [formDefaultDisk, setFormDefaultDisk] = useState("")

  const resetForm = () => {
    setFormProvider("replicated")
    setFormToken("")
    setFormApiUrl("")
    setFormApiKey("")
    setFormDefaultTtl("")
    setFormDefaultInstanceType("")
    setFormDefaultSnapshot("")
    setFormDefaultCpu("")
    setFormDefaultMemory("")
    setFormDefaultDisk("")
    setEditName(null)
  }

  const openAdd = () => {
    resetForm()
    const firstAvailable = SANDBOX_PROVIDER_OPTIONS.find(o => !providers[o.value])
    if (firstAvailable) {
      setFormProvider(firstAvailable.value)
      if (firstAvailable.value === "exedev") {
        setFormDefaultCpu("2")
        setFormDefaultMemory("4")
        setFormDefaultDisk("10")
      }
    }
    setModalMode("add")
    setShowModal(true)
  }
  const openEdit = (name: string) => {
    const p = providers[name]
    const opt = SANDBOX_PROVIDER_OPTIONS.find(o => o.value === name)
    setFormProvider(name)
    setFormToken("")
    setFormApiUrl(p?.apiUrl || "")
    setFormApiKey("")
    setFormDefaultTtl(p?.defaultTtl || "")
    setFormDefaultInstanceType(p?.defaultInstanceType || "")
    setFormDefaultSnapshot(p?.defaultSnapshot || "")
    setFormDefaultCpu(p?.defaultCpu?.toString() || "")
    setFormDefaultMemory(p?.defaultMemory ? p.defaultMemory.replace(/GB$/, "") : "")
    setFormDefaultDisk(p?.defaultDisk ? p.defaultDisk.replace(/GB$/, "") : "")
    setEditName(name)
    setModalMode("edit")
    setShowModal(true)
  }

  const availableProviders = SANDBOX_PROVIDER_OPTIONS.filter(o => !providers[o.value])

  function doSave() {
    const patch: Record<string, unknown> = {}
    if (formProvider === "replicated") {
      if (formDefaultTtl) patch.defaultTtl = formDefaultTtl
      if (formDefaultInstanceType) patch.defaultInstanceType = formDefaultInstanceType
      if (formToken) patch.token = formToken
    } else if (formProvider === "daytona") {
      if (formApiUrl) patch.apiUrl = formApiUrl
      if (formApiKey) patch.apiKey = formApiKey
      if (formDefaultSnapshot) patch.defaultSnapshot = formDefaultSnapshot
    } else if (formProvider === "exedev") {
      // exe.dev uses SSH key authentication; no API key needed in config
      patch.enabled = true
      const parsedCpu = parseInt(formDefaultCpu, 10)
      if (formDefaultCpu && !isNaN(parsedCpu)) patch.defaultCpu = parsedCpu
      if (formDefaultMemory) patch.defaultMemory = formDefaultMemory + "GB"
      if (formDefaultDisk) patch.defaultDisk = formDefaultDisk + "GB"
    }
    onSave({ providers: { [formProvider]: patch } })
    setShowModal(false)
    resetForm()
  }

  function doRemove(name: string) {
    onSave({ providers: { [name]: { delete: true } } })
    setShowModal(false)
    resetForm()
  }

  const modalTitle = modalMode === "add"
    ? "Add Sandbox Provider"
    : `Edit ${SANDBOX_PROVIDER_OPTIONS.find(o => o.value === formProvider)?.label || formProvider}`

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Sandbox Runtimes</h2>
        <p className="text-sm text-muted-foreground mb-4">Configure VM providers for spawning claws.</p>

        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {configuredProviders.length} provider{configuredProviders.length !== 1 ? "s" : ""} configured
          </span>
        </div>
      </div>

      {/* Configured providers list */}
      {configuredProviders.length > 0 && (
        <div className="space-y-2 mb-4">
          {configuredProviders.map((p) => (
            <div
              key={p.name}
              onClick={() => openEdit(p.name)}
              className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
            >
              <div className="flex items-center gap-3">
                <div className="w-8 h-8 rounded-lg bg-muted flex items-center justify-center">
                  <Cpu className="size-4 text-muted-foreground" />
                </div>
                <div>
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium">{p.label}</span>
                    {p.configured && (
                      <span className="text-xs bg-green-500/20 text-green-400 px-1.5 py-0.5 rounded flex items-center gap-1">
                        <CheckCircle2 className="size-3" /> Configured
                      </span>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground">{p.description}</p>
                </div>
              </div>
              <span className="text-muted-foreground text-lg">⋯</span>
            </div>
          ))}
        </div>
      )}

      {configuredProviders.length === 0 && (
        <div className="border border-dashed border-border rounded-lg p-8 text-center space-y-3 mb-4">
          <div className="w-10 h-10 rounded-full bg-muted flex items-center justify-center mx-auto">
            <Cpu className="size-5 text-muted-foreground" />
          </div>
          <p className="text-sm text-muted-foreground">No sandbox providers configured</p>
        </div>
      )}

      {availableProviders.length > 0 && (
        <Button onClick={openAdd} className="gap-2">
          <span className="text-sm">+</span> Add Sandbox Provider
        </Button>
      )}

      {/* SSH Keys — global, stays at bottom */}
      <div className="border-t border-border pt-6 mt-6">
        <p className="text-xs font-medium mb-2">Additional SSH Keys</p>
        <p className="text-xs text-muted-foreground mb-3">Public keys injected into VMs at bootstrap for direct SSH access.</p>
        {(settings.sshPublicKeys || []).map((key, i) => {
          const parts = key.trim().split(/\s+/)
          const keyType = parts[0] || ""
          const comment = parts[2] || ""
          const keyBody = parts[1] || ""
          const shortKey = keyBody.length > 12 ? keyBody.slice(0, 8) + "..." + keyBody.slice(-4) : keyBody
          return (
            <div key={i} className="flex items-center gap-2 mb-2">
              <div className="flex-1 min-w-0">
                <code className="text-xs font-mono">{keyType} {shortKey}</code>
                {comment && <span className="ml-2 text-xs text-muted-foreground">{comment}</span>}
              </div>
              <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive px-2 h-7" disabled={saving}
                onClick={() => onSave({ sshPublicKeys: (settings.sshPublicKeys || []).filter((_, j) => j !== i) })}>
                Remove
              </Button>
            </div>
          )
        })}
        <div className="flex gap-2">
          <Input value={newKey} onChange={e => setNewKey(e.target.value)}
            onKeyDown={e => { if (e.key === "Enter" && newKey.trim()) { onSave({ sshPublicKeys: [...(settings.sshPublicKeys || []), newKey.trim()] }); setNewKey("") }}}
            placeholder="ssh-ed25519 AAAA..." className="h-7 text-xs font-mono flex-1" />
          <Button size="sm" disabled={saving || !newKey.trim()}
            onClick={() => { onSave({ sshPublicKeys: [...(settings.sshPublicKeys || []), newKey.trim()] }); setNewKey("") }}>
            Add
          </Button>
        </div>
      </div>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetForm() }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">{modalTitle}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{modalTitle}</h3>
          </div>

          <div className="p-5 space-y-4">
            {modalMode === "add" && (
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Provider</label>
                <select
                  value={formProvider}
                  onChange={e => setFormProvider(e.target.value)}
                  className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm"
                >
                  {availableProviders.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                </select>
              </div>
            )}

            {formProvider === "replicated" && (
              <>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                  <Input
                    type="password"
                    value={formToken}
                    onChange={e => setFormToken(e.target.value)}
                    className="h-8 text-sm"
                    placeholder={modalMode === "edit" && providers.replicated?.tokenSet ? "Leave blank to keep existing" : "Enter Replicated API token"}
                  />
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Default TTL</label>
                    <Input value={formDefaultTtl} onChange={e => setFormDefaultTtl(e.target.value)} className="h-8 text-sm" placeholder="48h" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Default Instance</label>
                    <Input value={formDefaultInstanceType} onChange={e => setFormDefaultInstanceType(e.target.value)} className="h-8 text-sm" placeholder="r1.large" />
                  </div>
                </div>
              </>
            )}

            {formProvider === "daytona" && (
              <>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">API URL</label>
                  <Input value={formApiUrl} onChange={e => setFormApiUrl(e.target.value)} className="h-8 text-sm" placeholder="https://app.daytona.io" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
                  <Input
                    type="password"
                    value={formApiKey}
                    onChange={e => setFormApiKey(e.target.value)}
                    className="h-8 text-sm"
                    placeholder={modalMode === "edit" && providers.daytona?.apiKeySet ? "Leave blank to keep existing" : "Enter Daytona API key"}
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default Snapshot</label>
                  <Input value={formDefaultSnapshot} onChange={e => setFormDefaultSnapshot(e.target.value)} className="h-8 text-sm" placeholder="daytona-medium" />
                </div>
              </>
            )}

            {formProvider === "exedev" && (
              <>
                <div className="bg-muted/50 rounded-lg p-3 space-y-2">
                  <p className="text-xs font-medium">SSH Key Setup</p>
                  <p className="text-xs text-muted-foreground">
                    {modalMode === "edit"
                      ? <>A key pair has been generated for exe.dev. Add this public key to your{" "}<a href="https://exe.dev" target="_blank" rel="noopener noreferrer" className="underline">exe.dev account</a>.</>
                      : <>An SSH key pair will be generated automatically when you save. You can copy the public key from the edit view and add it to your{" "}<a href="https://exe.dev" target="_blank" rel="noopener noreferrer" className="underline">exe.dev account</a>.</>}
                  </p>
                  {modalMode === "edit" && providers.exedev?.sshPublicKey ? (
                    <div className="flex items-center gap-2">
                      <code className="text-xs font-mono bg-background px-2 py-1 rounded flex-1 truncate">
                        {providers.exedev.sshPublicKey}
                      </code>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 px-2"
                        onClick={() => {
                          navigator.clipboard.writeText(providers.exedev.sshPublicKey || "")
                          setCopiedKey(true)
                          setTimeout(() => setCopiedKey(false), 2000)
                        }}
                      >
                        {copiedKey ? <Check className="size-3 text-green-500" /> : <Copy className="size-3" />}
                      </Button>
                    </div>
                  ) : (
                    <p className="text-xs text-muted-foreground italic">
                      Public key will be shown after saving.
                    </p>
                  )}
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default CPUs</label>
                  <Input
                    type="number"
                    min={1}
                    max={32}
                    value={formDefaultCpu}
                    onChange={e => setFormDefaultCpu(e.target.value)}
                    className="h-8 text-sm"
                    placeholder="2"
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default Memory (GB)</label>
                  <Input
                    type="number"
                    min={1}
                    max={128}
                    value={formDefaultMemory}
                    onChange={e => setFormDefaultMemory(e.target.value)}
                    className="h-8 text-sm"
                    placeholder="4"
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default Disk (GB)</label>
                  <Input
                    type="number"
                    min={10}
                    max={500}
                    value={formDefaultDisk}
                    onChange={e => setFormDefaultDisk(e.target.value)}
                    className="h-8 text-sm"
                    placeholder="10"
                  />
                </div>
              </>
            )}
          </div>

          <div className="flex items-center justify-between px-5 py-4 border-t border-border">
            {modalMode === "edit" && editName && (
              <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => doRemove(editName)}>
                <Trash2 className="size-3.5 mr-1" /> Remove
              </Button>
            )}
            <div className="flex items-center gap-2 ml-auto">
              <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetForm() }}>Cancel</Button>
              <Button
                size="sm"
                disabled={saving || (modalMode === "add" && formProvider === "replicated" && !formToken) || (modalMode === "add" && formProvider === "daytona" && !formApiKey) || (modalMode === "add" && formProvider === "exedev" && (!formDefaultCpu || !formDefaultMemory || !formDefaultDisk))}
                onClick={doSave}
              >
                {modalMode === "add" ? "Add Provider" : "Save changes"}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

const PROVIDER_OPTIONS = [
  { value: "anthropic",  label: "Anthropic",  placeholder: "sk-ant-..." },
  { value: "fireworks",  label: "Fireworks",  placeholder: "fw_..." },
  { value: "openai",     label: "OpenAI",     placeholder: "sk-proj-..." },
  { value: "codex",      label: "Codex",      placeholder: "sk-proj-..." },
  { value: "ollama",     label: "Ollama",     placeholder: "ollama-local" },
  { value: "other",      label: "Other",      placeholder: "" },
]

const PROVIDER_MODELS: Record<string, { id: string; name: string }[]> = {
  anthropic: [
    { id: "anthropic/claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    { id: "anthropic/claude-opus-4-5",   name: "Claude Opus 4.5" },
    { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
    { id: "__custom",                    name: "Custom Anthropic model" },
  ],
  fireworks: [
    { id: "fireworks/accounts/fireworks/models/kimi-k2p6",                  name: "Kimi K2.6" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-pro",            name: "DeepSeek V4 Pro" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-flash",          name: "DeepSeek V4 Flash" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p7",               name: "MiniMax M2.7" },
    { id: "fireworks/accounts/fireworks/models/glm-5p1",                    name: "GLM 5.1" },
    { id: "fireworks/accounts/fireworks/models/qwen3p6-plus",               name: "Qwen3.6 Plus" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-120b",               name: "OpenAI gpt-oss-120b" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-20b",                name: "OpenAI gpt-oss-20b" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p5",               name: "MiniMax M2.5" },
    { id: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct",    name: "Llama 3.3 70B Instruct" },
    { id: "__custom",                                                       name: "Custom Fireworks model" },
  ],
  openai: [
    { id: "openai/gpt-5.5",      name: "GPT-5.5" },
    { id: "openai/gpt-5.5-pro",  name: "GPT-5.5 Pro" },
    { id: "openai/gpt-5.4",      name: "GPT-5.4" },
    { id: "openai/gpt-5.4-pro",  name: "GPT-5.4 Pro" },
    { id: "openai/gpt-5.4-mini", name: "GPT-5.4 Mini" },
    { id: "openai/o4-mini",      name: "o4-mini" },
    { id: "openai/o3",           name: "o3" },
    // coding-tuned variants available via regular OpenAI API
    { id: "openai/gpt-5.3-codex", name: "GPT-5.3 Codex (coding tuned)" },
    { id: "__custom",             name: "Custom OpenAI model" },
  ],
  codex: [
    // Codex is an autonomous agentic coding platform — it selects and routes to the
    // appropriate underlying model (including special Codex checkpoints) on its own.
    { id: "codex/codex",      name: "Codex (auto)" },
    { id: "codex/codex-pro",  name: "Codex Pro (auto)" },
    { id: "__custom",         name: "Custom Codex model" },
  ],
  ollama: [
    { id: "ollama/qwen2.5-coder:1.5b", name: "Qwen2.5 Coder 1.5B" },
    { id: "ollama/qwen2.5-coder:7b", name: "Qwen2.5 Coder 7B" },
    { id: "ollama/llama3.2:3b",      name: "Llama 3.2 3B" },
    { id: "ollama/gpt-oss:20b",      name: "gpt-oss 20B" },
    { id: "__custom",                name: "Custom Ollama model" },
  ],
}

function LLMSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const llmKeys = settings.llmKeys || []
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [editIdx, setEditIdx] = useState<number | null>(null)

  // Form state
  const [formName, setFormName] = useState("")
  const [formProvider, setFormProvider] = useState("anthropic")
  const [formCustomProvider, setFormCustomProvider] = useState("")
  const [formKey, setFormKey] = useState("")
  const [formDefault, setFormDefault] = useState(false)
  const [formDefaultModel, setFormDefaultModel] = useState("")
  const [formCustomModel, setFormCustomModel] = useState("")

  const providerLabel = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.label ?? p
  const providerPlaceholder = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.placeholder ?? ""

  const resetForm = () => {
    setFormName(""); setFormProvider("anthropic"); setFormCustomProvider(""); setFormKey(""); setFormDefault(false); setFormDefaultModel(""); setFormCustomModel(""); setEditIdx(null)
  }

  const openAdd = () => { resetForm(); setModalMode("add"); setShowModal(true) }
  const openEdit = (i: number) => {
    const k = llmKeys[i]
    setFormName(k.name)
    const isCustom = !PROVIDER_OPTIONS.some(o => o.value === k.provider)
    setFormProvider(isCustom ? "other" : k.provider)
    setFormCustomProvider(isCustom ? k.provider : "")
    setFormKey("")
    setFormDefault(k.default)
    const providerModels = PROVIDER_MODELS[k.provider] || []
    if (k.defaultModel && providerModels.length > 0 && !providerModels.some(m => m.id === k.defaultModel)) {
      setFormDefaultModel("__custom")
      setFormCustomModel(k.defaultModel)
    } else {
      setFormDefaultModel(k.defaultModel || "")
      setFormCustomModel("")
    }
    setEditIdx(i)
    setModalMode("edit")
    setShowModal(true)
  }

  const needsAttention = llmKeys.filter(k => !k.keySet).length
  const configuredCount = llmKeys.filter(k => k.keySet).length

  function doSave() {
    const actualProvider = formProvider === "other" ? formCustomProvider : formProvider
    const actualDefaultModel = formDefaultModel === "__custom" ? formCustomModel.trim() : formDefaultModel
    if (modalMode === "add") {
      if (!formName.trim() || (!formKey.trim() && actualProvider !== "ollama")) return
      onSave({ llmKeys: [{ name: formName.trim(), provider: actualProvider, apiKey: formKey.trim(), default: formDefault, defaultModel: actualDefaultModel || undefined }] })
    } else if (editIdx !== null) {
      const existing = llmKeys[editIdx]
      const patch: Record<string, unknown> = {
        name: existing.name,          // lookup key — original name
        newName: formName.trim(),      // new name if changed
      }
      if (formKey.trim()) patch.apiKey = formKey.trim()
      patch.default = formDefault       // always send so user can unset
      if (actualDefaultModel) patch.defaultModel = actualDefaultModel
      if (actualProvider !== existing.provider) patch.provider = actualProvider
      onSave({ llmKeys: [patch] })
    }
    setShowModal(false)
    resetForm()
  }

  function doRemove(i: number) {
    onSave({ llmKeys: [{ name: llmKeys[i].name, delete: true }] })
    setShowModal(false)
    resetForm()
  }

  function setDefault(i: number) {
    onSave({ llmKeys: [{ name: llmKeys[i].name, default: true }] })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Models</h2>
        <p className="text-sm text-muted-foreground mb-4">
          API keys for LLM providers. The default key is used unless overridden by a workflow.
        </p>

        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {llmKeys.length} key{llmKeys.length !== 1 ? "s" : ""} configured
          </span>
          {needsAttention > 0 && (
            <span className="text-xs bg-red-500/10 text-red-400 border border-red-500/20 px-2 py-1 rounded font-medium flex items-center gap-1">
              <AlertTriangle className="size-3" /> {needsAttention} need{needsAttention !== 1 ? "" : "s"} attention
            </span>
          )}
        </div>
      </div>

      {/* Needs Attention */}
      {needsAttention > 0 && (
        <div>
          <p className="text-sm font-medium text-yellow-400 mb-2 flex items-center gap-1.5">
            <AlertTriangle className="size-4" /> Needs Attention
          </p>
          <div className="space-y-2">
            {llmKeys.filter(k => !k.keySet).map((k) => (
              <div
                key={k.name}
                onClick={() => openEdit(llmKeys.indexOf(k))}
                className="border border-red-500/20 rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-red-500/5 transition-colors"
              >
                <div className="flex items-center gap-3">
                  <div className="w-8 h-8 rounded-lg bg-muted flex items-center justify-center">
                    <Zap className="size-4 text-muted-foreground" />
                  </div>
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{k.name}</span>
                      <span className="text-xs text-muted-foreground">{providerLabel(k.provider)}</span>
                    </div>
                    <div className="flex items-center gap-2 mt-0.5">
                      {k.defaultModel ? (
                        <span className="text-xs text-muted-foreground">model: {k.defaultModel}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">using provider default</span>
                      )}
                      <span className="text-xs text-red-400 flex items-center gap-1">✕ Invalid</span>
                    </div>
                  </div>
                </div>
                <span className="text-muted-foreground text-lg">⋯</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Configured */}
      {configuredCount > 0 && (
        <div>
          <p className="text-sm font-medium text-muted-foreground mb-2">Configured</p>
          <div className="space-y-2">
            {llmKeys.filter(k => k.keySet).map((k) => (
              <div
                key={k.name}
                onClick={() => openEdit(llmKeys.indexOf(k))}
                className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
              >
                <div className="flex items-center gap-3">
                  <div className="w-8 h-8 rounded-lg bg-muted flex items-center justify-center">
                    {k.provider === "anthropic" || k.provider === "openai" || k.provider === "codex" ? (
                      <Sparkles className="size-4 text-muted-foreground" />
                    ) : (
                      <Zap className="size-4 text-muted-foreground" />
                    )}
                  </div>
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{k.name}</span>
                      <span className="text-xs text-muted-foreground">{providerLabel(k.provider)}</span>
                      {k.default && (
                        <span className="text-xs bg-primary/20 text-primary px-1.5 py-0.5 rounded flex items-center gap-1">
                          <Sparkles className="size-3" /> Default
                        </span>
                      )}
                    </div>
                    <div className="flex items-center gap-2 mt-0.5">
                      {k.defaultModel ? (
                        <span className="text-xs text-muted-foreground">model: {k.defaultModel}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">using provider default</span>
                      )}
                      <span className="text-xs text-green-400 flex items-center gap-1">✓ Valid</span>
                    </div>
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  {!k.default && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 text-xs"
                      onClick={(e) => { e.stopPropagation(); setDefault(llmKeys.indexOf(k)) }}
                      disabled={saving}
                    >
                      Set default
                    </Button>
                  )}
                  <span className="text-muted-foreground text-lg">⋯</span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      <Button onClick={openAdd} className="gap-2">
        <span className="text-sm">+</span> Add Model Key
      </Button>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetForm() }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">{modalMode === "add" ? "Add Model Key" : `Edit ${formName}`}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{modalMode === "add" ? "Add Model Key" : `Edit ${formName}`}</h3>
          </div>

          <div className="p-5 space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                  <Input value={formName} onChange={e => setFormName(e.target.value)} className="h-8 text-sm" placeholder="anthropic-prod" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Provider</label>
                  <select
                    value={formProvider}
                    onChange={e => { setFormProvider(e.target.value); setFormDefaultModel(""); setFormCustomModel("") }}
                    className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm"
                  >
                    {PROVIDER_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                  </select>
                </div>
              </div>
              {formProvider === "other" && (
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Custom Provider Name</label>
                  <Input value={formCustomProvider} onChange={e => setFormCustomProvider(e.target.value)} className="h-8 text-sm" placeholder="mistral" />
                </div>
              )}
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
                <Input
                  type="password"
                  value={formKey}
                  onChange={e => setFormKey(e.target.value)}
                  className="h-8 text-sm"
                  placeholder={modalMode === "edit" ? "Leave blank to keep existing" : providerPlaceholder(formProvider)}
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default model <span className="text-muted-foreground/60">(optional)</span></label>
                {PROVIDER_MODELS[formProvider] ? (
                  <select
                    value={formDefaultModel}
                    onChange={e => setFormDefaultModel(e.target.value)}
                    className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
                  >
                    <option value="">— use provider default —</option>
                    {PROVIDER_MODELS[formProvider].map(m => (
                      <option key={m.id} value={m.id}>{m.name}</option>
                    ))}
                  </select>
                ) : (
                  <Input value={formDefaultModel} onChange={e => setFormDefaultModel(e.target.value)} className="h-8 text-sm" placeholder="e.g. myprovider/model-name" />
                )}
                {PROVIDER_MODELS[formProvider] && formDefaultModel === "__custom" && (
                  <Input
                    value={formCustomModel}
                    onChange={e => setFormCustomModel(e.target.value)}
                    className="h-8 text-sm mt-2"
                    placeholder={formProvider === "ollama" ? "e.g. ollama/qwen3:14b" : "e.g. provider/model-name"}
                  />
                )}
              </div>
              <label className="flex items-center gap-2 text-sm cursor-pointer">
                <input type="checkbox" checked={formDefault} onChange={e => setFormDefault(e.target.checked)} />
                Set as default key
              </label>
            </div>

            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => doRemove(editIdx)}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetForm() }}>Cancel</Button>
                <Button
                  size="sm"
                  disabled={saving || !formName.trim() || (modalMode === "add" && !formKey.trim()) || (formProvider === "other" && !formCustomProvider.trim())}
                  onClick={doSave}
                >
                  {modalMode === "add" ? "Add Model Key" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function GitHubSection({ settings, onSave, saving, workspace }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean; workspace: string }) {
  const [showModal, setShowModal] = useState(false)
  const [appName, setAppName] = useState("")
  const [appId, setAppId] = useState("")
  const [url, setUrl] = useState("")
  const [installation, setInstallation] = useState("")
  const [pem, setPem] = useState("")
  const [testResult, setTestResult] = useState<GitHubAppView | null>(null)
  const [testing, setTesting] = useState(false)
  const [testError, setTestError] = useState("")
  const [workspaceApps, setWorkspaceApps] = useState<WorkspaceGitHubAppView[]>([])
  const [workspaceLoading, setWorkspaceLoading] = useState(true)
  const [workspaceError, setWorkspaceError] = useState("")
  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const resetModal = () => {
    setAppName(""); setAppId(""); setUrl(""); setInstallation(""); setPem("")
    setTestResult(null); setTestError(""); setTesting(false)
  }

  const openModal = () => { resetModal(); setShowModal(true) }
  const closeModal = () => { setShowModal(false); resetModal() }

  const workspaceGitHubPath = workspace ? `/api/workspaces/${encodeURIComponent(workspace)}/github-apps` : ""
  const loadWorkspaceApps = useCallback(async () => {
    if (!workspace) return
    setWorkspaceLoading(true)
    setWorkspaceError("")
    try {
      const res = await fetch(`${hubUrl}${workspaceGitHubPath}`, { headers: { Authorization: `Bearer ${token()}` } })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setWorkspaceApps(data.githubApps || [])
    } catch (e) {
      setWorkspaceError(e instanceof Error ? e.message : "Failed to load GitHub Apps")
    } finally {
      setWorkspaceLoading(false)
    }
  }, [hubUrl, workspace, workspaceGitHubPath])

  useEffect(() => {
    loadWorkspaceApps()
  }, [loadWorkspaceApps])

  async function runTest() {
    setTesting(true); setTestError(""); setTestResult(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/github/test`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ appId: parseInt(appId, 10), url, privateKeyPem: pem }),
      })
      if (!res.ok) {
        const txt = await res.text()
        throw new Error(txt || `HTTP ${res.status}`)
      }
      setTestResult(await res.json())
    } catch (e) {
      setTestError(e instanceof Error ? e.message : "Test failed")
    } finally {
      setTesting(false)
    }
  }

  const [showConfirmModal, setShowConfirmModal] = useState(false)

  async function doSave(force = false) {
    // Not tested yet — recommend testing
    if (!force && testResult === null) {
      setShowConfirmModal(true)
      return
    }
    // Tested and failed — warn but allow
    if (!force && testResult && testResult.permCheckOk !== true) {
      setShowConfirmModal(true)
      return
    }
    const parsedAppId = parseInt(appId, 10)
    if (workspace) {
      setWorkspaceError("")
      const name = appName.trim() || `app-${parsedAppId}`
      const res = await fetch(`${hubUrl}${workspaceGitHubPath}`, {
        method: "PUT",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ name, appId: parsedAppId, url, installation, privateKeyPem: pem }),
      })
      if (!res.ok) {
        setWorkspaceError(await res.text())
        return
      }
      closeModal()
      await loadWorkspaceApps()
      return
    }
    const newApp: { appId: number; privateKeyPem: string; url?: string } = { appId: parsedAppId, privateKeyPem: pem }
    if (url) newApp.url = url
    onSave({ github: [...(settings.github || []), newApp] })
    closeModal()
  }

  async function deleteWorkspaceApp(name: string) {
    if (!workspace) return
    setWorkspaceError("")
    const res = await fetch(`${hubUrl}${workspaceGitHubPath}?name=${encodeURIComponent(name)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token()}` },
    })
    if (!res.ok) {
      setWorkspaceError(await res.text())
      return
    }
    await loadWorkspaceApps()
  }

  const needsAttention = testResult?.permissions?.filter(p => !p.ok).length ?? 0
  const configuredCount = testResult?.permissions?.filter(p => p.ok).length ?? 0
  const totalCount = testResult?.permissions?.length ?? 0

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">GitHub Apps</h2>
      <div className="text-sm text-muted-foreground mb-6 space-y-1.5">
        <p>
          Register a GitHub App so your ElasticClaw workflows can access repositories.
          When a claw is created, it gets a scoped token that can read and write code,
          open pull requests, and check CI status — but only on repos the App is installed on.
        </p>
        <p>
          The App needs <strong>contents:write</strong>, <strong>pull_requests:write</strong>,
          and read access to <strong>metadata</strong>, <strong>checks</strong>, and <strong>statuses</strong>.
          Install it on your org or specific repos, then add the App ID and private key here.
        </p>
      </div>

      {workspaceError && <p className="mb-4 text-sm text-destructive">{workspaceError}</p>}

      {workspace ? (
        <div className="mb-6 space-y-2">
          {workspaceLoading ? (
            <p className="text-sm text-muted-foreground animate-pulse">Loading GitHub Apps...</p>
          ) : workspaceApps.length === 0 ? (
            <p className="text-sm text-muted-foreground">No GitHub Apps configured for this workspace.</p>
          ) : (
            workspaceApps.map(app => (
              <div key={app.name} className="border border-border rounded-lg p-4">
                <div className="flex items-center justify-between">
                  <div>
                    <p className="text-sm font-medium">{app.name}</p>
                    <p className="text-xs text-muted-foreground">App ID: {app.appId}</p>
                    {app.url && (
                      <a href={app.url} target="_blank" rel="noopener noreferrer" className="text-xs text-muted-foreground hover:text-primary flex items-center gap-1">
                        {app.url} <ExternalLink className="size-3" />
                      </a>
                    )}
                    {app.installation && <p className="text-xs text-muted-foreground">Installation: {app.installation}</p>}
                  </div>
                  <div className="flex items-center gap-2">
                    <span className={cn("text-xs px-2 py-1 rounded", (app.privateKeySet || app.private_key_set) ? "bg-green-500/20 text-green-400" : "bg-yellow-500/20 text-yellow-400")}>
                      {(app.privateKeySet || app.private_key_set) ? "Key set" : "No key"}
                    </span>
                    <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive h-7 px-2" disabled={saving}
                      onClick={() => deleteWorkspaceApp(app.name)}>
                      <Trash2 className="size-3.5" />
                    </Button>
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      ) : settings.github?.length > 0 && (
        <div className="mb-6 space-y-2">
          {settings.github.map(app => (
            <div key={app.appId} className="border border-border rounded-lg p-4">
              <div className="flex items-center justify-between mb-2">
                <div>
                  <p className="text-sm font-medium">App ID: {app.appId}</p>
                  {app.url && (
                    <a href={app.url} target="_blank" rel="noopener noreferrer" className="text-xs text-muted-foreground hover:text-primary flex items-center gap-1">
                      {app.url} <ExternalLink className="size-3" />
                    </a>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  <span className={cn("text-xs px-2 py-1 rounded", app.keySet ? "bg-green-500/20 text-green-400" : "bg-yellow-500/20 text-yellow-400")}>
                    {app.keySet ? "Key set" : "No key"}
                  </span>
                  <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive h-7 px-2" disabled={saving}
                    onClick={() => {
                      const filtered = settings.github.filter(a => a.appId !== app.appId).map(a => {
                        const item: { appId: number; url?: string } = { appId: a.appId }
                        if (a.url) item.url = a.url
                        return item
                      })
                      onSave({ github: filtered })
                    }}>
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              </div>

              {/* Permission check results */}
              {app.permCheckError && (
                <div className="mt-3 rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-start gap-2">
                  <AlertTriangle className="size-4 text-yellow-400 shrink-0 mt-0.5" />
                  <div>
                    <p className="text-xs font-medium text-yellow-400">Permission check failed</p>
                    <p className="text-xs text-yellow-400/80">{app.permCheckError}</p>
                  </div>
                </div>
              )}
              {app.permissions && app.permissions.length > 0 && (
                <div className="mt-3">
                  {app.permCheckOk === false && (
                    <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 mb-2 flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <Shield className="size-4 text-yellow-400" />
                        <div>
                          <p className="text-xs font-medium text-yellow-400">
                            {app.permissions.filter(p => !p.ok).length} permission{app.permissions.filter(p => !p.ok).length !== 1 ? "s" : ""} need attention
                          </p>
                          <p className="text-xs text-yellow-400/70">
                            {app.permissions.filter(p => p.ok).length} of {app.permissions.length} required permissions granted
                          </p>
                        </div>
                      </div>
                      {app.url && (
                        <a href={app.url} target="_blank" rel="noopener noreferrer">
                          <Button size="sm" variant="outline" className="h-7 text-xs gap-1">
                            <ExternalLink className="size-3" /> Fix in GitHub
                          </Button>
                        </a>
                      )}
                    </div>
                  )}

                  <details className="group" open={app.permCheckOk === false}>
                    <summary className="flex items-center gap-2 text-xs font-medium text-muted-foreground cursor-pointer list-none">
                      <Shield className="size-3.5" />
                      Required Permissions
                      <ChevronLeft className="size-3 transition-transform group-open:-rotate-90" />
                    </summary>

                    {app.permissions.filter(p => !p.ok).length > 0 && (
                      <div className="mt-2 space-y-1">
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Needs Attention</p>
                        {app.permissions.filter(p => !p.ok).map(p => (
                          <div key={p.name} className="flex items-center justify-between rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2">
                            <div className="flex items-center gap-2">
                              <AlertTriangle className="size-3.5 text-yellow-400" />
                              <span className="text-sm font-mono text-yellow-400">{p.name}</span>
                            </div>
                            <div className="flex items-center gap-1.5 text-xs">
                              <span className="px-1.5 py-0.5 rounded bg-yellow-500/20 text-yellow-400">{p.granted || "not set"}</span>
                              <span className="text-muted-foreground">→</span>
                              <span className="px-1.5 py-0.5 rounded border border-yellow-500/30 text-yellow-400">needs {p.needed}</span>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}

                    {app.permissions.filter(p => p.ok).length > 0 && (
                      <div className="mt-2 space-y-1">
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Configured</p>
                        {app.permissions.filter(p => p.ok).map(p => (
                          <div key={p.name} className="flex items-center justify-between rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2">
                            <div className="flex items-center gap-2">
                              <CheckCircle2 className="size-3.5 text-green-400" />
                              <span className="text-sm font-mono text-green-400">{p.name}</span>
                            </div>
                            <span className="text-xs px-1.5 py-0.5 rounded bg-green-500/20 text-green-400">{p.granted}</span>
                          </div>
                        ))}
                      </div>
                    )}
                  </details>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Button onClick={openModal} className="gap-2">
        <Github className="size-4" /> Add GitHub App
      </Button>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { if (!open) closeModal(); else setShowModal(true) }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">Add GitHub App</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">Add GitHub App</h3>
          </div>

          <div className="p-5 space-y-4">
              <div className="grid grid-cols-2 gap-3">
                {workspace && (
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                    <Input value={appName} onChange={e => setAppName(e.target.value)} className="h-8 text-sm" placeholder="primary" />
                  </div>
                )}
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App ID</label>
                  <Input type="number" value={appId} onChange={e => setAppId(e.target.value)} className="h-8 text-sm" placeholder="123456" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App URL (optional)</label>
                  <Input value={url} onChange={e => setUrl(e.target.value)} className="h-8 text-sm" placeholder="https://github.com/apps/..." />
                </div>
                {workspace && (
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Installation (optional)</label>
                    <Input value={installation} onChange={e => setInstallation(e.target.value)} className="h-8 text-sm" placeholder="owner or org" />
                  </div>
                )}
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Private Key (PEM)</label>
                <textarea
                  value={pem}
                  onChange={e => setPem(e.target.value)}
                  placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;...&#10;-----END RSA PRIVATE KEY-----"
                  className="w-full h-32 rounded-md border border-border bg-background px-3 py-2 text-xs font-mono resize-none focus:outline-none focus:ring-2 focus:ring-ring"
                />
              </div>

              <div className="flex gap-2">
                <Button size="sm" variant="outline" disabled={testing || !appId || !pem || isNaN(Number(appId))} onClick={runTest} className="gap-1">
                  {testing ? <RotateCcw className="size-3 animate-spin" /> : <Check className="size-3" />}
                  Test Permissions
                </Button>
              </div>

              {testError && (
                <div className="rounded-lg bg-red-500/10 border border-red-500/20 px-3 py-2.5 flex items-start gap-2">
                  <AlertTriangle className="size-4 text-red-400 shrink-0 mt-0.5" />
                  <p className="text-xs text-red-400">{testError}</p>
                </div>
              )}

              {/* Test result — show SOMETHING for every result state */}
              {testResult && (
                <div className="space-y-3">
                  {/* permCheckOk is true — all good */}
                  {testResult.permCheckOk === true && (
                    <div className="rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2.5 flex items-center gap-2">
                      <CheckCircle2 className="size-4 text-green-400" />
                      <div>
                        <p className="text-xs font-medium text-green-400">All {totalCount} required permissions granted</p>
                        <p className="text-xs text-green-400/70">This GitHub App is ready to use.</p>
                      </div>
                    </div>
                  )}

                  {/* permCheckOk is false or missing — show the problem */}
                  {(testResult.permCheckOk === false || testResult.permCheckOk == null) && (
                    <>
                      {testResult.permCheckError ? (
                        <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-start gap-2">
                          <AlertTriangle className="size-4 text-yellow-400 shrink-0 mt-0.5" />
                          <p className="text-xs text-yellow-400">{testResult.permCheckError}</p>
                        </div>
                      ) : (
                        <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-center justify-between">
                          <div className="flex items-center gap-2">
                            <Shield className="size-4 text-yellow-400" />
                            <div>
                              <p className="text-xs font-medium text-yellow-400">
                                {needsAttention} permission{needsAttention !== 1 ? "s" : ""} need attention
                              </p>
                              <p className="text-xs text-yellow-400/70">
                                {configuredCount} of {totalCount} required permissions granted
                              </p>
                            </div>
                          </div>
                          {url && (
                            <a href={url} target="_blank" rel="noopener noreferrer">
                              <Button size="sm" variant="outline" className="h-7 text-xs gap-1">
                                <ExternalLink className="size-3" /> Fix in GitHub
                              </Button>
                            </a>
                          )}
                        </div>
                      )}
                    </>
                  )}

                  {/* Permission list — shown whenever we have permissions */}
                  {testResult.permissions && testResult.permissions.length > 0 && (
                    <div className="space-y-1">
                      {testResult.permissions.filter(p => !p.ok).length > 0 && (
                        <>
                          <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Needs Attention</p>
                          {testResult.permissions.filter(p => !p.ok).map(p => (
                            <div key={p.name} className="flex items-center justify-between rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2">
                              <div className="flex items-center gap-2">
                                <AlertTriangle className="size-3.5 text-yellow-400" />
                                <span className="text-sm font-mono text-yellow-400">{p.name}</span>
                              </div>
                              <div className="flex items-center gap-1.5 text-xs">
                                <span className="px-1.5 py-0.5 rounded bg-yellow-500/20 text-yellow-400">{p.granted || "not set"}</span>
                                <span className="text-muted-foreground">→</span>
                                <span className="px-1.5 py-0.5 rounded border border-yellow-500/30 text-yellow-400">needs {p.needed}</span>
                              </div>
                            </div>
                          ))}
                        </>
                      )}
                      {testResult.permissions.filter(p => p.ok).length > 0 && (
                        <>
                          <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium mt-2">Configured</p>
                          {testResult.permissions.filter(p => p.ok).map(p => (
                            <div key={p.name} className="flex items-center justify-between rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2">
                              <div className="flex items-center gap-2">
                                <CheckCircle2 className="size-3.5 text-green-400" />
                                <span className="text-sm font-mono text-green-400">{p.name}</span>
                              </div>
                              <span className="text-xs px-1.5 py-0.5 rounded bg-green-500/20 text-green-400">{p.granted}</span>
                            </div>
                          ))}
                        </>
                      )}
                    </div>
                  )}
                </div>
              )}
            </div>

            <div className="flex items-center justify-end gap-2 px-5 py-4 border-t border-border">
              <Button size="sm" variant="outline" onClick={closeModal}>Cancel</Button>
              <Button size="sm" disabled={saving || !appId || !pem || isNaN(Number(appId))} onClick={() => doSave(false)}>
                Save
              </Button>
            </div>
        </DialogContent>
      </Dialog>

      {/* Confirm save modal — not tested or test failed */}
      <Dialog open={showConfirmModal} onOpenChange={open => { if (!open) setShowConfirmModal(false) }}>
        <DialogContent className="max-w-md p-0 gap-0">
          <DialogTitle className="sr-only">Confirm GitHub App Save</DialogTitle>
          <div className="p-5 space-y-4">
            {testResult === null ? (
              <>
                <div className="flex items-center gap-2">
                  <AlertTriangle className="size-5 text-yellow-400" />
                  <h3 className="font-medium">Test Recommended</h3>
                </div>
                <p className="text-sm text-muted-foreground">
                  You haven't tested this GitHub App yet. We recommend clicking <strong>Test Permissions</strong> first to verify it works.
                </p>
              </>
            ) : (
              <>
                <div className="flex items-center gap-2">
                  <AlertTriangle className="size-5 text-yellow-400" />
                  <h3 className="font-medium">Permissions Missing</h3>
                </div>
                <p className="text-sm text-muted-foreground">
                  This GitHub App is missing required permissions. It <strong>will not work</strong> for claws until fixed.
                </p>
                {testResult.permCheckError && (
                  <p className="text-xs text-yellow-400 bg-yellow-500/10 rounded px-2 py-1.5">{testResult.permCheckError}</p>
                )}
              </>
            )}
            <div className="flex items-center justify-end gap-2 pt-2">
              <Button size="sm" variant="outline" onClick={() => setShowConfirmModal(false)}>Go Back</Button>
              <Button size="sm" variant="secondary" onClick={() => { setShowConfirmModal(false); doSave(true) }}>
                Save Anyway
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function AuthenticationSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const ghOAuth = settings.auth?.githubOAuth
  const ghAccess = settings.auth?.access
  const ghAllowedUsers = ghOAuth?.allowedUsers || []
  const ghAllowedOrgs = ghOAuth?.allowedOrgs || []
  const ghAllowedTeams = ghOAuth?.allowedTeams || []
  const ghAdmins = ghAccess?.admins || []
  const ghViewRequiresTags = ghAccess?.viewRequiresTags || []
  const ghInteractRequiresTags = ghAccess?.interactRequiresTags || []

  // Password card state
  const [newPw, setNewPw] = useState('')
  const [pwConfirm, setPwConfirm] = useState('')
  const [pwErr, setPwErr] = useState('')

  // GitHub OAuth config state
  const [showGhForm, setShowGhForm] = useState(false)
  const [clientId, setClientId] = useState(ghOAuth?.clientId || '')
  const [clientSecret, setClientSecret] = useState('')
  const [allowedUsers, setAllowedUsers] = useState(ghAllowedUsers.join(', '))
  const [allowedOrgs, setAllowedOrgs] = useState(ghAllowedOrgs.join(', '))
  const [allowedTeams, setAllowedTeams] = useState(ghAllowedTeams.join(', '))
  const [admins, setAdmins] = useState(ghAdmins.join(', '))
  const [viewTags, setViewTags] = useState(ghViewRequiresTags.join(', '))
  const [interactTags, setInteractTags] = useState(ghInteractRequiresTags.join(', '))
  const [ghErr, setGhErr] = useState('')

  function handlePasswordSave() {
    setPwErr('')
    if (newPw.length < 8) { setPwErr('Password must be at least 8 characters'); return }
    if (newPw !== pwConfirm) { setPwErr('Passwords do not match'); return }
    onSave({ uiPassword: newPw })
    setNewPw(''); setPwConfirm('')
  }

  function splitList(s: string) {
    return s.split(/[,\n]+/).map((t: string) => t.trim()).filter(Boolean)
  }

  function handleGitHubSave() {
    setGhErr('')
    if (!clientId.trim()) { setGhErr('Client ID is required'); return }
    if (!ghOAuth && !clientSecret.trim()) { setGhErr('Client Secret is required for initial setup'); return }
    onSave({
      auth: {
        githubOAuth: {
          clientId: clientId.trim(),
          ...(clientSecret ? { clientSecret: clientSecret.trim() } : {}),
          allowedUsers: splitList(allowedUsers),
          allowedOrgs: splitList(allowedOrgs),
          allowedTeams: splitList(allowedTeams),
        },
        access: {
          admins: splitList(admins),
          viewRequiresTags: splitList(viewTags),
          interactRequiresTags: splitList(interactTags),
        },
      }
    })
    setClientSecret('')
    setShowGhForm(false)
  }

  function handleGitHubRemove() {
    if (!window.confirm('Disable GitHub OAuth? Users will only be able to log in with the password.')) return
    onSave({ auth: { removeGithubOAuth: true } })
  }

  function handleGitHubEdit() {
    setClientId(ghOAuth?.clientId || '')
    setClientSecret('')
    setAllowedUsers(ghAllowedUsers.join(', '))
    setAllowedOrgs(ghAllowedOrgs.join(', '))
    setAllowedTeams(ghAllowedTeams.join(', '))
    setAdmins(ghAdmins.join(', '))
    setViewTags(ghViewRequiresTags.join(', '))
    setInteractTags(ghInteractRequiresTags.join(', '))
    setShowGhForm(true)
  }

  const callbackUrl = typeof window !== 'undefined'
    ? window.location.origin + '/login'
    : 'https://your-hub/login'

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-0.5">Authentication</h2>
        <p className="text-sm text-muted-foreground">Both methods can be active simultaneously. GitHub OAuth users and password users both have full access unless tag-based restrictions are configured.</p>
      </div>

      {/* Password card */}
      <div className="border border-border rounded-lg p-5 space-y-3 max-w-lg">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium">Password Login</h3>
            <p className="text-xs text-muted-foreground mt-0.5">
              {settings.auth?.disablePasswordAuth ? 'Disabled — GitHub OAuth only.' : 'Enabled. Used by the hub token and UI password.'}
            </p>
          </div>
          {settings.auth?.disablePasswordAuth
            ? <span className="text-xs bg-muted text-muted-foreground px-2 py-0.5 rounded font-medium">Disabled</span>
            : <span className="text-xs bg-green-500/20 text-green-400 px-2 py-0.5 rounded font-medium">Active</span>
          }
        </div>

        {/* Disable/enable toggle — only show when GitHub OAuth is configured */}
        {ghOAuth && (
          <div className="flex items-center justify-between border border-border rounded-md px-3 py-2">
            <div>
              <p className="text-xs font-medium">Require GitHub OAuth</p>
              <p className="text-xs text-muted-foreground mt-0.5">Disable password login entirely. Make sure you can log in via GitHub first.</p>
            </div>
            <button
              onClick={() => onSave({ auth: { disablePasswordAuth: !settings.auth?.disablePasswordAuth } })}
              disabled={saving}
              className={cn(
                'relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors',
                settings.auth?.disablePasswordAuth ? 'bg-primary' : 'bg-muted'
              )}
            >
              <span className={cn(
                'pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-lg transform transition-transform',
                settings.auth?.disablePasswordAuth ? 'translate-x-4' : 'translate-x-0'
              )} />
            </button>
          </div>
        )}

        {!settings.auth?.disablePasswordAuth && (
          <div className="border-t border-border pt-3 space-y-3">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">New Password</label>
              <Input type="password" value={newPw} onChange={e => setNewPw(e.target.value)} className="h-8 text-sm" placeholder="Min 8 characters" />
            </div>
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Confirm Password</label>
              <Input type="password" value={pwConfirm} onChange={e => setPwConfirm(e.target.value)} className="h-8 text-sm" placeholder="Repeat password" />
            </div>
            {pwErr && <p className="text-xs text-red-500">{pwErr}</p>}
            <Button size="sm" disabled={saving || !newPw || !pwConfirm} onClick={handlePasswordSave}>
              Change Password
            </Button>
          </div>
        )}
      </div>

      {/* GitHub OAuth card */}
      <div className="border border-border rounded-lg p-5 space-y-3 max-w-lg">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium">GitHub OAuth</h3>
            <p className="text-xs text-muted-foreground mt-0.5">Let users log in with their GitHub account.</p>
          </div>
          {ghOAuth
            ? <span className="text-xs bg-green-500/20 text-green-400 px-2 py-0.5 rounded font-medium">Active</span>
            : <span className="text-xs bg-muted text-muted-foreground px-2 py-0.5 rounded font-medium">Inactive</span>
          }
        </div>

        {ghOAuth && !showGhForm && (
          <div className="border-t border-border pt-3 space-y-2 text-xs text-muted-foreground">
            <div className="flex gap-2"><span className="font-medium text-foreground w-28">Client ID</span><span className="font-mono">{ghOAuth.clientId}</span></div>
            <div className="flex gap-2"><span className="font-medium text-foreground w-28">Client Secret</span><span>{ghOAuth.clientSecretSet ? '••••••••' : 'not set'}</span></div>
            {ghAdmins.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Admins</span><span>{ghAdmins.join(', ')}</span></div>}
            {ghAllowedUsers.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed users</span><span>{ghAllowedUsers.join(', ')}</span></div>}
            {ghAllowedOrgs.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed orgs</span><span>{ghAllowedOrgs.join(', ')}</span></div>}
            {ghAllowedTeams.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed teams</span><span>{ghAllowedTeams.join(', ')}</span></div>}
            {ghViewRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">View tag filter</span><span className="font-mono">{ghViewRequiresTags.join(', ')}</span></div>}
            {ghInteractRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Interact tag filter</span><span className="font-mono">{ghInteractRequiresTags.join(', ')}</span></div>}
            <div className="flex gap-2 pt-1">
              <Button size="sm" variant="outline" onClick={handleGitHubEdit}>Edit</Button>
              <Button size="sm" variant="outline" onClick={handleGitHubRemove} className="text-destructive hover:text-destructive">Disable</Button>
            </div>
          </div>
        )}

        {(!ghOAuth || showGhForm) && (
          <div className="border-t border-border pt-3 space-y-4">
            {!ghOAuth && (
              <p className="text-xs text-muted-foreground">
                Create a <a href="https://github.com/settings/developers" target="_blank" rel="noopener noreferrer" className="underline">GitHub OAuth App</a> and set the callback URL to{' '}
                <code className="bg-muted px-1 rounded">{callbackUrl}</code>.
              </p>
            )}

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">App Credentials</h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Client ID</label>
                <Input value={clientId} onChange={e => setClientId(e.target.value)} className="h-8 text-sm font-mono" placeholder="Ov23li..." />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">
                  Client Secret{ghOAuth?.clientSecretSet && <span className="ml-1 text-green-500">(set)</span>}
                </label>
                <Input
                  type="password"
                  value={clientSecret}
                  onChange={e => setClientSecret(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder={ghOAuth?.clientSecretSet ? 'Leave blank to keep existing' : 'Paste secret...'}
                />
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Allowlist <span className="normal-case font-normal">(leave blank = any GitHub user)</span></h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Usernames</label>
                <Input value={allowedUsers} onChange={e => setAllowedUsers(e.target.value)} className="h-8 text-sm" placeholder="alice, bob" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Orgs</label>
                <Input value={allowedOrgs} onChange={e => setAllowedOrgs(e.target.value)} className="h-8 text-sm" placeholder="my-org" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Teams</label>
                <Input value={allowedTeams} onChange={e => setAllowedTeams(e.target.value)} className="h-8 text-sm" placeholder="my-org/my-team" />
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Admins</h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">GitHub Usernames</label>
                <Input value={admins} onChange={e => setAdmins(e.target.value)} className="h-8 text-sm" placeholder="alice, bob" />
                <p className="text-xs text-muted-foreground mt-1">Comma-separated. Admins can access Settings and bypass all tag restrictions.</p>
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Tag-Based Access Control <span className="normal-case font-normal">(optional)</span></h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">View requires tag</label>
                <Input value={viewTags} onChange={e => setViewTags(e.target.value)} className="h-8 text-sm font-mono" placeholder="user, team=frontend" />
                <p className="text-xs text-muted-foreground mt-1">Claw must have a tag like <code className="bg-muted px-1 rounded">user=alice</code> for that user to see it.</p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Interact requires tag</label>
                <Input value={interactTags} onChange={e => setInteractTags(e.target.value)} className="h-8 text-sm font-mono" placeholder="user, team=frontend" />
              </div>
            </div>

            {ghErr && <p className="text-xs text-red-500">{ghErr}</p>}

            <div className="flex gap-2">
              <Button size="sm" disabled={saving} onClick={handleGitHubSave}>
                {ghOAuth ? 'Save Changes' : 'Enable GitHub OAuth'}
              </Button>
              {showGhForm && (
                <Button size="sm" variant="outline" onClick={() => { setShowGhForm(false); setGhErr('') }}>Cancel</Button>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
type TrackerType = "linear" | "shortcut" | "github-issues"

interface TrackerItem {
  type: TrackerType
  workspace: string
  tokenSet: boolean
}

function IntegrationsSection({ settings, onSave, saving, selectedWorkspace, hubPublicUrl }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean; selectedWorkspace: string; hubPublicUrl: string }) {
  const [workspaceTrackers, setWorkspaceTrackers] = useState<TrackerItem[]>([])
  const [githubOwnerHint, setGithubOwnerHint] = useState("")
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const hubUrl = getHubUrl()
  const authToken = () => getAuthToken() || ""
  const issueTrackersPath = selectedWorkspace ? `/api/workspaces/${encodeURIComponent(selectedWorkspace)}/issue-trackers` : ""
  const linear = workspaceTrackers.filter(t => t.type === "linear")
  const shortcut = workspaceTrackers.filter(t => t.type === "shortcut")
  const githubIssues = workspaceTrackers.filter(t => t.type === "github-issues")

  const allTrackers: TrackerItem[] = workspaceTrackers

  const loadTrackers = useCallback(async () => {
    if (!selectedWorkspace) return
    setLoading(true)
    setError("")
    try {
      const res = await fetch(`${hubUrl}${issueTrackersPath}`, { headers: { Authorization: `Bearer ${authToken()}` } })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setWorkspaceTrackers(data.issueTrackers || [])
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load issue trackers")
    } finally {
      setLoading(false)
    }
  }, [hubUrl, issueTrackersPath, selectedWorkspace])

  useEffect(() => {
    loadTrackers()
  }, [loadTrackers])

  useEffect(() => {
    if (!selectedWorkspace) return
    async function loadGitHubOwnerHint() {
      try {
        const res = await fetch(`${hubUrl}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/github-apps`, { headers: { Authorization: `Bearer ${authToken()}` } })
        if (!res.ok) return
        const data = await res.json()
        const apps = (data.githubApps || []) as WorkspaceGitHubAppView[]
        const owners = new Set<string>()
        for (const app of apps) {
          for (const owner of app.installations || []) {
            if (owner) owners.add(owner)
          }
        }
        setGithubOwnerHint(owners.size === 1 ? Array.from(owners)[0] : "")
      } catch {
        setGithubOwnerHint("")
      }
    }
    loadGitHubOwnerHint()
  }, [hubUrl, selectedWorkspace])

  // Unified modal state
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [modalType, setModalType] = useState<TrackerType>("linear")
  const [editIdx, setEditIdx] = useState<number | null>(null)
  const [editType, setEditType] = useState<TrackerType>("linear")
  const [token, setToken] = useState("")
  const [webhookSecret, setWebhookSecret] = useState("")
  const [copiedSetup, setCopiedSetup] = useState<string | null>(null)
  const [setupTab, setSetupTab] = useState<"token" | "webhook">("token")
  const [showAddMenu, setShowAddMenu] = useState(false)
  const addMenuRef = useRef<HTMLDivElement>(null)

  // Close add menu on outside click
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (addMenuRef.current && !addMenuRef.current.contains(e.target as Node)) {
        setShowAddMenu(false)
      }
    }
    if (showAddMenu) {
      document.addEventListener("mousedown", handleClick)
      return () => document.removeEventListener("mousedown", handleClick)
    }
  }, [showAddMenu])

  const resetModal = () => {
    setToken(""); setWebhookSecret(""); setEditIdx(null); setEditType("linear"); setSetupTab("token")
  }

  const openAdd = (type: TrackerType) => {
    resetModal()
    setModalType(type)
    setModalMode("add")
    setShowModal(true)
    setShowAddMenu(false)
  }

  const openEdit = (tracker: TrackerItem, idx: number) => {
    setToken("")
    setWebhookSecret("")
    setEditIdx(idx)
    setEditType(tracker.type)
    setSetupTab("token")
    setModalMode("edit")
    setShowModal(true)
  }

  async function saveTracker() {
    if (modalMode === "add" && !token.trim()) return

    const type = modalMode === "add" ? modalType : editType
    const trackerWorkspace = selectedWorkspace
    const originalWorkspace = modalMode === "edit" && editIdx !== null ? allTrackers[editIdx]?.workspace : ""
    setError("")
    const res = await fetch(`${hubUrl}${issueTrackersPath}`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${authToken()}`, "Content-Type": "application/json" },
      body: JSON.stringify({ type, workspace: trackerWorkspace, originalWorkspace, token: token.trim(), webhookSecret: webhookSecret.trim() }),
    })
    if (!res.ok) {
      setError(await res.text())
      return
    }
    setShowModal(false)
    resetModal()
    await loadTrackers()
  }

  async function removeTracker() {
    if (editIdx === null) return
    const tracker = allTrackers[editIdx]
    if (!tracker) return
    setError("")
    const res = await fetch(`${hubUrl}${issueTrackersPath}?type=${encodeURIComponent(tracker.type)}&workspace=${encodeURIComponent(tracker.workspace)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${authToken()}` },
    })
    if (!res.ok) {
      setError(await res.text())
      return
    }
    setShowModal(false)
    resetModal()
    await loadTrackers()
  }

  const trackerTypeLabel = (t: TrackerType) => {
    switch (t) {
      case "linear": return "Linear"
      case "shortcut": return "Shortcut"
      case "github-issues": return "GitHub Issues"
    }
  }

  const modalTitle = modalMode === "add"
    ? `Add ${trackerTypeLabel(modalType)}`
    : `Edit ${trackerTypeLabel(editType)}`

  const modalIcon = (modalMode === "add" ? modalType : editType) === "linear"
    ? <Zap className="size-4" />
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <span className="text-[#F4603C]">⚡</span>
    : <Github className="size-4" />

  const githubIssuesTokenParams = new URLSearchParams({
    name: "ElasticClaw GitHub Issues",
    description: "Allows ElasticClaw to read and update GitHub issues",
    expires_in: "90",
    issues: "write",
    metadata: "read",
  })
  if (githubOwnerHint) {
    githubIssuesTokenParams.set("target_name", githubOwnerHint)
  }
  const githubIssuesTokenUrl = `https://github.com/settings/personal-access-tokens/new?${githubIssuesTokenParams.toString()}`
  const tokenHint = (modalMode === "add" ? modalType : editType) === "linear"
    ? <>Use a Linear API key from <a href="https://linear.app/settings/account/security" target="_blank" rel="noopener noreferrer" className="underline">linear.app/settings/account/security</a>.</>
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <>Use a Shortcut API token from Shortcut settings. The token lets ElasticClaw read and update stories.</>
    : (modalMode === "add" ? modalType : editType) === "github-issues"
    ? <>Use a <a href={githubIssuesTokenUrl} target="_blank" rel="noopener noreferrer" className="underline">fine-grained GitHub PAT</a> for issue API actions. {githubOwnerHint ? <>The link starts with <code>{githubOwnerHint}</code> as the resource owner. </> : null}Grant repository access to the repos this workspace watches.</>
    : null
  const activeTrackerType = modalMode === "add" ? modalType : editType
  const canGenerateWebhookSecret = activeTrackerType === "github-issues" || activeTrackerType === "shortcut"
  const workspaceWebhookBase = hubPublicUrl || hubUrl
  const linearWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/linear`
  const githubIssuesWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/github-issues`

  function generateWebhookSecret() {
    const bytes = new Uint8Array(32)
    crypto.getRandomValues(bytes)
    setWebhookSecret(Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join(""))
  }

  function copySetupValue(value: string, key: string) {
    navigator.clipboard.writeText(value).then(() => {
      setCopiedSetup(key)
      setTimeout(() => setCopiedSetup(null), 2000)
    })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Issue Trackers</h2>
        <p className="text-sm text-muted-foreground mb-4">Connect issue trackers to sync and create issues from workflows.</p>

        {/* Summary badges */}
        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {allTrackers.length} tracker{allTrackers.length !== 1 ? "s" : ""} connected
          </span>
          {linear.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Linear: {linear.length}</span>
          )}
          {shortcut.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Shortcut: {shortcut.length}</span>
          )}
          {githubIssues.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">GitHub Issues: {githubIssues.length}</span>
          )}
        </div>
      </div>

      {error && <p className="text-sm text-destructive">{error}</p>}

      {/* Configured trackers list */}
      {loading ? (
        <p className="text-sm text-muted-foreground animate-pulse">Loading issue trackers...</p>
      ) : allTrackers.length > 0 && (
        <div className="space-y-2 mb-4">
          {allTrackers.map((tracker, i) => (
            <div
              key={`${tracker.type}-${tracker.workspace}`}
              onClick={() => openEdit(tracker, i)}
              className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
            >
              <div className="flex items-center gap-3">
                {tracker.type === "linear" ? (
                  <Zap className="size-4 text-muted-foreground" />
                ) : tracker.type === "shortcut" ? (
                  <span className="text-[#F4603C]">⚡</span>
                ) : (
                  <Github className="size-4 text-muted-foreground" />
                )}
                <div>
                  <p className="text-sm font-medium">{tracker.workspace}</p>
                  <div className="flex items-center gap-1.5 mt-0.5">
                    <span className="text-xs text-muted-foreground capitalize">{tracker.type === "github-issues" ? "github issues" : tracker.type}</span>
                    <span className="text-xs text-muted-foreground">·</span>
                    {tracker.tokenSet ? (
                      <span className="text-xs text-green-400 flex items-center gap-1">
                        <CheckCircle2 className="size-3" /> Connected
                      </span>
                    ) : (
                      <span className="text-xs text-red-400 flex items-center gap-1">
                        <AlertTriangle className="size-3" /> Token revoked
                      </span>
                    )}
                  </div>
                </div>
              </div>
              <span className="text-muted-foreground text-lg">⋯</span>
            </div>
          ))}
        </div>
      )}

      {!loading && allTrackers.length === 0 && (
        <div className="border border-dashed border-border rounded-lg p-8 text-center space-y-3 mb-4">
          <div className="w-10 h-10 rounded-full bg-muted flex items-center justify-center mx-auto">
            <Zap className="size-5 text-muted-foreground" />
          </div>
          <p className="text-sm text-muted-foreground">No issue trackers connected</p>
        </div>
      )}

      {/* Add dropdown */}
      <div className="relative" ref={addMenuRef}>
        <Button size="sm" variant="outline" onClick={() => setShowAddMenu(!showAddMenu)} className="gap-1">
          <span className="text-sm">+</span> Add issue tracker
        </Button>
        {showAddMenu && (
          <div className="absolute top-full left-0 mt-1 bg-background border border-border rounded-lg shadow-lg py-1 z-50 min-w-[180px]">
            <button
              onClick={() => openAdd("linear")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <Zap className="size-4" />
              <span>Linear</span>
            </button>
            <button
              onClick={() => openAdd("shortcut")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <span className="text-[#F4603C]">⚡</span>
              <span>Shortcut</span>
            </button>
            <button
              onClick={() => openAdd("github-issues")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <Github className="size-4" />
              <span>GitHub Issues</span>
            </button>
          </div>
        )}
      </div>

      {/* Unified Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetModal() }}>
        <DialogContent className={cn("p-0 gap-0", activeTrackerType === "github-issues" || activeTrackerType === "linear" ? "sm:max-w-5xl w-[min(1100px,calc(100vw-2rem))]" : "max-w-lg")}>
          <DialogTitle className="sr-only">{modalTitle}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <div className="flex items-center gap-2">
              {modalIcon}
              <h3 className="font-medium">{modalTitle}</h3>
            </div>
          </div>
          <div className="p-5 space-y-4">
              {activeTrackerType === "github-issues" || activeTrackerType === "linear" ? (
                <div className="grid min-h-[420px] grid-cols-[240px_1fr]">
                  <div className="-ml-5 -my-5 border-r border-border p-4">
                    <div className="space-y-1">
                      <button
                        type="button"
                        onClick={() => setSetupTab("token")}
                        className={cn(
                          "w-full rounded-lg px-3 py-2 text-left text-sm transition-colors",
                          setupTab === "token" ? "bg-primary/10 text-primary font-medium" : "text-muted-foreground hover:bg-secondary hover:text-foreground"
                        )}
                      >
                        {activeTrackerType === "github-issues" ? "GitHub PAT" : "API Token"}
                      </button>
                      <button
                        type="button"
                        onClick={() => setSetupTab("webhook")}
                        className={cn(
                          "w-full rounded-lg px-3 py-2 text-left text-sm transition-colors",
                          setupTab === "webhook" ? "bg-primary/10 text-primary font-medium" : "text-muted-foreground hover:bg-secondary hover:text-foreground"
                        )}
                      >
                        {activeTrackerType === "github-issues" ? "Webhook" : "Webhook Secret"}
                      </button>
                    </div>
                  </div>
                  <div className="pl-5">
                    <p className="text-sm text-muted-foreground">
                      {activeTrackerType === "github-issues"
                        ? "Connect GitHub Issues for this workspace. This is separate from the GitHub App used for repo checkout tokens."
                        : "Connect Linear for this workspace. The API key lets ElasticClaw read issues and move them between statuses."}
                    </p>
                    {setupTab === "token" ? (
                    <div className="mt-4 space-y-4">
                      <div>
                        <h4 className="text-sm font-medium">{activeTrackerType === "github-issues" ? "GitHub PAT" : "Linear API Token"}</h4>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? "Used by ElasticClaw to read and update issues."
                            : "Used by ElasticClaw to read Linear issues and move them between statuses."}
                        </p>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                        <Input type="password" value={token} onChange={e => setToken(e.target.value)} className="h-9 text-sm" placeholder={`${trackerTypeLabel(activeTrackerType)} API token`} />
                        {tokenHint && <p className="text-xs text-muted-foreground mt-1">{tokenHint}</p>}
                      </div>
                    </div>
                    ) : (
                    <div className="mt-4 space-y-4">
                      <div>
                        <h4 className="text-sm font-medium">Webhook</h4>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? "Create a repo or org webhook for Issues events."
                            : "Create a Linear webhook for Issue events, then paste its signing secret below if you configured one."}
                        </p>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">Payload URL</label>
                        <div className="flex gap-2">
                          <Input readOnly value={activeTrackerType === "github-issues" ? githubIssuesWebhookUrl : linearWebhookUrl} className="h-9 text-xs font-mono" />
                          <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(activeTrackerType === "github-issues" ? githubIssuesWebhookUrl : linearWebhookUrl, `${activeTrackerType}-url`)}>
                            {copiedSetup === `${activeTrackerType}-url` ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                        </div>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
                        <div className="flex gap-2">
                          <Input type="password" value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)} className="h-9 text-sm" placeholder="Webhook secret" />
                          <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(webhookSecret, `${activeTrackerType}-secret`)} disabled={!webhookSecret}>
                            {copiedSetup === `${activeTrackerType}-secret` ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                          {activeTrackerType === "github-issues" && (
                            <Button type="button" size="sm" variant="outline" onClick={generateWebhookSecret}>
                              Generate
                            </Button>
                          )}
                        </div>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? "Paste the same secret into the GitHub webhook Secret field."
                            : "Copy the signing secret from the Linear webhook settings and paste it here. Leave blank only if you intentionally want unsigned Linear webhooks."}
                        </p>
                      </div>
                    </div>
                    )}
                  </div>
                </div>
              ) : activeTrackerType === "shortcut" ? (
                <div className="space-y-2 text-sm text-muted-foreground">
                  <p>Connect Shortcut for this workspace. The API token lets ElasticClaw read stories and update workflow states.</p>
                  <p>Create a Shortcut webhook using the Shortcut URL from the Webhooks page. If Shortcut signs the payload with a secret, paste that same secret below.</p>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">
                  Connect {trackerTypeLabel(activeTrackerType)} for this workspace.
                </p>
              )}
              {activeTrackerType !== "github-issues" && activeTrackerType !== "linear" && (
                <>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                <Input type="password" value={token} onChange={e => setToken(e.target.value)} className="h-9 text-sm" placeholder={`${trackerTypeLabel(activeTrackerType)} API token`} />
                {tokenHint && <p className="text-xs text-muted-foreground mt-1">{tokenHint}</p>}
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
                <div className="flex gap-2">
                  <Input type="password" value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)} className="h-9 text-sm" placeholder="Webhook secret for signature verification" />
                  {canGenerateWebhookSecret && (
                    <Button type="button" size="sm" variant="outline" onClick={generateWebhookSecret}>
                      Generate
                    </Button>
                  )}
                </div>
                <p className="text-xs text-muted-foreground mt-1">
                  {activeTrackerType === "shortcut"
                    ? "Generate one here, then use the same value when configuring the Shortcut webhook signature secret."
                    : "Used to verify incoming webhook signatures. Leave blank to keep existing."}
                </p>
              </div>
                </>
              )}
            </div>
            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={removeTracker}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetModal() }}>Cancel</Button>
                <Button size="sm" disabled={saving || (modalMode === "add" && !token.trim())} onClick={saveTracker}>
                  {modalMode === "add" ? "Add tracker" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}



function WebhooksSection({ hubUrl, selectedWorkspace }: { hubUrl: string; selectedWorkspace: string }) {
  const [copied, setCopied] = useState<string | null>(null)
  const workspaceSlug = encodeURIComponent(selectedWorkspace)
  const workspaceWebhookBase = hubUrl ? `${hubUrl}/api/workspaces/${workspaceSlug}/webhooks` : ""

  const urls = [
    {
      name: "Linear",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/linear` : "",
      hint: "Paste into Linear → Settings → API → Webhooks, subscribe to Issue events.",
    },
    {
      name: "Shortcut",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/shortcut` : "",
      hint: "Use Shortcut's API to register this webhook: POST /api/v3/webhooks with this URL.",
    },
    {
      name: "GitHub Issues",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/github-issues` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Issues events.",
    },
    {
      name: "GitHub (PRs)",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/github` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Pull requests and Issue comments.",
    },
    {
      name: "External",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/external` : "",
      hint: "Use for generic signed webhook events and release events.",
    },
  ]

  const doCopy = (text: string, label: string) => {
    if (!text) return
    navigator.clipboard.writeText(text).then(() => {
      setCopied(label)
      setTimeout(() => setCopied(null), 2000)
    })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Webhooks</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Use these URLs to send events into the selected workspace.
        </p>
      </div>

      <div className="space-y-5">
        {urls.map(({ name, url, hint }) => (
          <div key={name} className="border border-border rounded-lg p-4 space-y-3">
            <div>
              <h4 className="text-sm font-medium">{name}</h4>
              <p className="text-xs text-muted-foreground mt-1">{hint}</p>
            </div>
            <div className="flex items-center gap-2">
              <code className="flex-1 text-xs font-mono bg-muted px-3 py-2 rounded-md border border-border truncate">
                {url || "Loading…"}
              </code>
              <Button variant="outline" size="sm" className="shrink-0" onClick={() => doCopy(url, name)} disabled={!url}>
                {copied === name ? <Check className="size-3.5 text-green-500" /> : <Copy className="size-3.5" />}
                <span className="ml-1.5">{copied === name ? "Copied" : "Copy"}</span>
              </Button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function WorkspacesSection({ selectedWorkspace }: { selectedWorkspace: string }) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setWorkspaces(await fetchWorkspaces())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load workspaces")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const visibleWorkspaces = workspaces.filter((workspace) => workspace.name === selectedWorkspace)

  return (
    <div className="space-y-6">
      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading && visibleWorkspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading workspace…</p>
      ) : workspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workspaces pushed yet.</p>
      ) : visibleWorkspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workspace named {selectedWorkspace} is configured.</p>
      ) : (
        <div className="space-y-4">
          {visibleWorkspaces.map((workspace) => (
            <div key={workspace.name} className="space-y-4">
              <h2 className="text-base font-semibold">{workspace.name}</h2>
              <div className="overflow-hidden rounded-md border border-border bg-muted/40">
                <YamlHighlight code={workspace.config || "No elasticclaw-config.yaml content available."} />
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function WorkflowsSection({ selectedWorkspace }: { selectedWorkspace: string }) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [savingWorkflow, setSavingWorkflow] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setWorkspaces(await fetchWorkspaces())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load workflows")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const workflows = workspaces.flatMap((workspace) =>
    (workspace.workflows || []).map((workflow) => ({ ...workflow, workspaceName: workflow.workspaceName || workspace.name }))
  ).filter((workflow) => workflow.workspaceName === selectedWorkspace)

  const patchWorkflow = useCallback(async (workflow: Workflow, patch: { enabled?: boolean; enableManualTrigger?: boolean }) => {
    const key = `${workflow.workspaceName}/${workflow.name}`
    setSavingWorkflow(key)
    setError("")
    try {
      const updated = await updateWorkflowControls(workflow, patch)
      setWorkspaces(current => current.map(workspace => {
        if (workspace.name !== updated.workspaceName) return workspace
        return {
          ...workspace,
          workflows: (workspace.workflows || []).map(item =>
            item.name === updated.name ? { ...item, ...updated } : item
          ),
        }
      }))
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to update workflow")
    } finally {
      setSavingWorkflow("")
    }
  }, [])

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Workflows</h2>
        <p className="text-sm text-muted-foreground">
          Workflows define triggers and runtime behavior within this workspace.
        </p>
      </div>

      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading && workflows.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading workflows…</p>
      ) : workflows.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workflows configured.</p>
      ) : (
        <div className="border border-border rounded-lg divide-y divide-border">
          {workflows.map((workflow) => (
            <WorkflowSummaryRow
              key={`${workflow.workspaceName}/${workflow.name}`}
              workflow={workflow}
              saving={savingWorkflow === `${workflow.workspaceName}/${workflow.name}`}
              onPatch={patchWorkflow}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function WorkflowSummaryRow({
  workflow,
  saving,
  onPatch,
}: {
  workflow: Workflow
  saving: boolean
  onPatch: (workflow: Workflow, patch: { enabled?: boolean; enableManualTrigger?: boolean }) => void
}) {
  return (
    <div className="px-4 py-3">
      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div className="min-w-0">
          <p className="text-sm font-medium truncate">{workflow.name}</p>
          <p className="text-xs text-muted-foreground truncate">
            {workflow.workspaceName} · {workflow.integration || "manual"}
            {workflow.triggerStatus ? ` · ${workflow.triggerStatus}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 shrink-0">
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              checked={workflow.enabled}
              disabled={saving}
              onCheckedChange={(checked) => onPatch(workflow, { enabled: checked })}
              aria-label={`Toggle automatic handling for ${workflow.name}`}
            />
            <span>Auto handling</span>
          </label>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              checked={Boolean(workflow.enableManualTrigger)}
              disabled={saving}
              onCheckedChange={(checked) => onPatch(workflow, { enableManualTrigger: checked })}
              aria-label={`Toggle manual trigger for ${workflow.name}`}
            />
            <span>Manual trigger</span>
          </label>
          <span className={cn(
            "text-xs px-2 py-0.5 rounded",
            workflow.enabled ? "bg-muted text-muted-foreground" : "bg-amber-500/10 text-amber-500"
          )}>
            {workflow.enabled ? "enabled" : "paused"}
          </span>
        </div>
      </div>
    </div>
  )
}

interface AnalyticsSummary {
  factoryName: string
  totalTriggers: number
  successfulCreations: number
  failedCreations: number
  terminations: number
  prOpened: number
  prMerged: number
  prClosed: number
  doneSignals: number
  errors: number
  successRate: number
  prMergeRate: number
}

function AnalyticsSection({ selectedWorkspace }: { selectedWorkspace?: string }) {
  const [summaries, setSummaries] = useState<AnalyticsSummary[]>([])
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const hubUrl = getHubUrl()
      const token = getAuthToken() || ""
      const [analyticsRes, workspaceData] = await Promise.all([
        fetch(`${hubUrl}/api/analytics/factories`, { headers: { Authorization: `Bearer ${token}` } }),
        fetchWorkspaces(),
      ])
      if (!analyticsRes.ok) throw new Error(await analyticsRes.text())
      setSummaries(await analyticsRes.json())
      setWorkspaces(workspaceData)
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load analytics")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const scopedToWorkspace = selectedWorkspace !== undefined
  const workflowNames = new Set(
    scopedToWorkspace && selectedWorkspace
      ? workspaces.find((workspace) => workspace.name === selectedWorkspace)?.workflows.map((workflow) => workflow.name) || []
      : []
  )
  const visibleSummaries = scopedToWorkspace
    ? summaries.filter((summary) => workflowNames.has(summary.factoryName))
    : summaries
  const totalTriggers = visibleSummaries.reduce((sum, item) => sum + item.totalTriggers, 0)
  const successfulCreations = visibleSummaries.reduce((sum, item) => sum + item.successfulCreations, 0)
  const failedCreations = visibleSummaries.reduce((sum, item) => sum + item.failedCreations, 0)
  const prMerged = visibleSummaries.reduce((sum, item) => sum + item.prMerged, 0)

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Analytics</h2>
        <p className="text-sm text-muted-foreground">
          {scopedToWorkspace ? "Activity for workflows in this workspace." : "Activity across all workspaces."}
        </p>
      </div>

      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading analytics…</p>
      ) : visibleSummaries.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No analytics yet.</p>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-4">
            <AnalyticsMetric label="Triggers" value={totalTriggers} />
            <AnalyticsMetric label="Created" value={successfulCreations} />
            <AnalyticsMetric label="Failed" value={failedCreations} />
            <AnalyticsMetric label="PRs merged" value={prMerged} />
          </div>
          <div className="border border-border rounded-lg divide-y divide-border">
            {visibleSummaries.map((summary) => (
              <div key={summary.factoryName} className="px-4 py-3">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="text-sm font-medium truncate">{summary.factoryName}</p>
                    <p className="text-xs text-muted-foreground">
                      {summary.totalTriggers} triggers · {summary.successRate.toFixed(0)}% create success · {summary.prMergeRate.toFixed(0)}% PR merge
                    </p>
                  </div>
                  <div className="shrink-0 text-right text-xs text-muted-foreground">
                    {summary.errors} errors
                  </div>
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  )
}

function AnalyticsMetric({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-lg border border-border p-3">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="text-xl font-semibold mt-1">{value}</p>
    </div>
  )
}

function WorkspaceRepositoryList({ values }: { values: RepositoryAccess[] }) {
  return (
    <div className="space-y-1">
      <h4 className="text-xs font-medium text-muted-foreground">Repositories</h4>
      {values.length === 0 ? (
        <p className="text-xs text-muted-foreground/70">None</p>
      ) : (
        <div className="space-y-1">
          {values.slice(0, 4).map((value) => (
            <div key={value.repo} className="flex items-center justify-between gap-2 text-xs bg-muted px-2 py-1 rounded">
              <code className="truncate">{value.repo}</code>
              <span className="shrink-0 text-muted-foreground">{value.permissions || "read"}</span>
            </div>
          ))}
          {values.length > 4 && (
            <p className="text-xs text-muted-foreground">+{values.length - 4} more</p>
          )}
        </div>
      )}
    </div>
  )
}

function WorkspaceAccessList({ title, values }: { title: string; values: string[] }) {
  return (
    <div className="space-y-1">
      <h4 className="text-xs font-medium text-muted-foreground">{title}</h4>
      {values.length === 0 ? (
        <p className="text-xs text-muted-foreground/70">None</p>
      ) : (
        <div className="space-y-1">
          {values.slice(0, 4).map((value) => (
            <code key={value} className="block text-xs bg-muted px-2 py-1 rounded truncate">
              {value}
            </code>
          ))}
          {values.length > 4 && (
            <p className="text-xs text-muted-foreground">+{values.length - 4} more</p>
          )}
        </div>
      )}
    </div>
  )
}

function SecretsSection({ settings, workspace }: { settings: SettingsData | null; workspace: string }) {
  const [secrets, setSecrets] = useState<string[]>([])
  const [newName, setNewName] = useState("")
  const [newValue, setNewValue] = useState("")
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""
  const secretsPath = workspace ? `/api/workspaces/${encodeURIComponent(workspace)}/secrets` : "/api/secrets"

  const refresh = useCallback(async () => {
    try {
      const res = await fetch(`${hubUrl}${secretsPath}`, { headers: { Authorization: `Bearer ${token()}` } })
      if (res.ok) {
        const data = await res.json()
        setSecrets(data.secrets || [])
      }
    } finally {
      setLoading(false)
    }
  }, [hubUrl, secretsPath])

  useEffect(() => {
    refresh()
  }, [refresh])

  const handleAdd = async () => {
    if (!newName.trim() || !newValue.trim()) return
    setSaving(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}${secretsPath}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token()}` },
        body: JSON.stringify({ name: newName.trim(), value: newValue.trim() }),
      })
      if (!res.ok) { setError(await res.text()); return }
      setNewName("")
      setNewValue("")
      await refresh()
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async (name: string) => {
    setError(null)
    const res = await fetch(`${hubUrl}${secretsPath}?name=${encodeURIComponent(name)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token()}` },
    })
    if (!res.ok) { setError(await res.text()); return }
    await refresh()
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Secrets</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Named secrets for workspace <code className="bg-muted px-1 rounded text-xs">{workspace || "default"}</code>. Values are stored on the hub and referenced from workspace env or workflow secret refs.
        </p>
      </div>

      <div className="border border-border rounded-lg divide-y divide-border">
        {loading ? (
          <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading secrets…</p>
        ) : secrets.length === 0 ? (
          <p className="text-sm text-muted-foreground px-4 py-6 text-center">No secrets configured.</p>
        ) : (
          secrets.map(name => (
            <div key={name} className="flex items-center justify-between px-4 py-3">
              <code className="text-sm font-mono">{name}</code>
              <Button variant="ghost" size="icon" className="text-muted-foreground hover:text-destructive" onClick={() => handleDelete(name)}>
                <Trash2 className="size-4" />
              </Button>
            </div>
          ))
        )}
      </div>

      <div className="border border-border rounded-lg p-5 space-y-4">
        <h3 className="text-sm font-medium">Add Secret</h3>
        <div className="flex gap-2">
          <Input placeholder="Name (e.g. linear_webhook_secret)" value={newName} onChange={e => setNewName(e.target.value)} className="font-mono text-sm" />
          <Input placeholder="Value" type="password" value={newValue} onChange={e => setNewValue(e.target.value)} className="font-mono text-sm" />
          <Button onClick={handleAdd} disabled={saving || !newName.trim() || !newValue.trim()}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </div>
        {error && <p className="text-xs text-destructive">{error}</p>}
      </div>
    </div>
  )
}

// ─── AI Config Section ───────────────────────────────────────────────────────

interface ChatMessage {
  role: "user" | "assistant"
  content: string
  streaming?: boolean
}

function normalizeStoredMessage(message: ChatMessage): ChatMessage {
  return {
    ...message,
    content: message.content.replace(/\u258c$/, ""),
    streaming: false,
  }
}

// Simple markdown renderer for assistant messages (no external deps)
// Strip ```yaml ... ``` blocks and any open/incomplete yaml code block at the end.
// Applied during streaming so YAML never appears in chat.
function stripYamlBlocks(text: string): string {
  // Remove complete yaml blocks
  let result = text.replace(/```ya?ml[\s\S]*?```/gi, "")
  // Remove any block that looks like a hub.yaml (complete)
  result = result.replace(/```[\s\S]*?```/g, (m) => {
    if (m.includes("claw_token:") || m.includes("url: http")) return ""
    return m
  })
  // Remove incomplete/open yaml block at the end (streaming: block started but not closed)
  result = result.replace(/```ya?ml[\s\S]*$/i, "")
  // Also remove any open ``` block at the end if it looks like hub.yaml
  result = result.replace(/```[\s\S]*$/g, (m) => {
    if (m.includes("claw_token:") || m.includes("url:") || m.includes("token:")) return ""
    return m
  })
  return result.trim()
}

function renderMarkdown(text: string): React.ReactNode[] {
  // Split off fenced code blocks first
  const parts = text.split(/(```[\s\S]*?```)/g)
  const nodes: React.ReactNode[] = []
  let globalKey = 0
  parts.forEach((part, pi) => {
    if (part.startsWith("```") && part.endsWith("```")) {
      const inner = part.slice(3, -3).replace(/^[^\n]*\n/, "") // strip language hint
      nodes.push(
        <pre key={`cb-${pi}`} className="bg-muted rounded p-2 my-1 overflow-x-auto">
          <code className="text-xs font-mono">{inner}</code>
        </pre>
      )
      return
    }
    // Process line by line
    const lines = part.split("\n")
    const lineNodes: React.ReactNode[] = []
    let ulItems: React.ReactNode[] = []
    let ulKey = 0
    const flushList = () => {
      if (ulItems.length > 0) {
        lineNodes.push(<ul key={`ul-${pi}-${ulKey++}`} className="list-disc pl-4 my-1 space-y-0.5">{ulItems}</ul>)
        ulItems = []
      }
    }
    lines.forEach((line, li) => {
      const isList = /^[\-\*]\s+/.test(line)
      if (!isList) flushList()
      if (isList) {
        ulItems.push(<li key={`li-${pi}-${li}`}>{inlineMarkdown(line.replace(/^[\-\*]\s+/, ""))}</li>)
      } else if (line.trim() === "") {
        lineNodes.push(<br key={`br-${pi}-${li}`} />)
      } else {
        lineNodes.push(<span key={`s-${pi}-${li}`}>{inlineMarkdown(line)}<br /></span>)
      }
    })
    flushList()
    nodes.push(...lineNodes.map((n, i) => React.cloneElement(n as React.ReactElement, { key: `n-${globalKey++}-${i}` })))
  })
  return nodes
}

function inlineMarkdown(text: string): React.ReactNode[] {
  // Split on inline code, bold, italic
  const tokens = text.split(/(``[^`]+``|`[^`]+`|\*\*[^*]+\*\*|\*[^*]+\*)/g)
  return tokens.map((tok, i) => {
    if (tok.startsWith("**") && tok.endsWith("**")) return <strong key={i}>{tok.slice(2, -2)}</strong>
    if (tok.startsWith("*") && tok.endsWith("*")) return <em key={i}>{tok.slice(1, -1)}</em>
    if (tok.startsWith("`") && tok.endsWith("`")) return <code key={i} className="bg-muted px-1 rounded text-xs font-mono">{tok.slice(1, -1)}</code>
    return tok
  })
}

function YamlHighlight({ code }: { code: string }) {
  const [html, setHtml] = useState<string | null>(null)
  useEffect(() => {
    if (!code) return
    import("shiki").then(({ codeToHtml }) =>
      codeToHtml(code, { lang: "yaml", theme: "github-dark" })
    ).then(setHtml).catch(() => setHtml(null))
  }, [code])
  if (!html) return <pre className="h-full overflow-auto p-3 text-xs font-mono leading-relaxed whitespace-pre">{code}</pre>
  return <div className="h-full overflow-auto p-3 text-xs leading-relaxed [&_pre]:!bg-transparent [&_code]:!text-xs [&_code]:!font-mono" dangerouslySetInnerHTML={{ __html: html }} />
}

// Session storage keys
const SS_CHAT_KEY = "ai-config-chat-history"
const SS_YAML_KEY = "ai-config-proposed-yaml"
const SS_BACKUP_KEY = "ai-config-backup-path"

function extractYamlPlaceholders(yaml: string): string[] {
  const seen = new Set<string>()
  const placeholders: string[] = []
  for (const match of yaml.matchAll(/__([A-Z0-9_]+)__/g)) {
    const name = match[1]
    if (name && !seen.has(name)) {
      seen.add(name)
      placeholders.push(name)
    }
  }
  return placeholders
}

function AIConfigSection() {
  // Load persisted state from sessionStorage — always start empty to avoid SSR hydration mismatch,
  // then restore from sessionStorage after mount.
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState("")
  const [loading, setLoading] = useState(false)
  const [proposedYaml, setProposedYaml] = useState<string | null>(null)
  const [currentConfig, setCurrentConfig] = useState<string | null>(null)
  const [placeholders, setPlaceholders] = useState<string[]>([])
  const [secretValues, setSecretValues] = useState<Record<string, string>>({})
  const [backupPath, setBackupPath] = useState<string | null>(null)
  // Restore from sessionStorage after mount (client-only)
  useEffect(() => {
    try {
      const raw = sessionStorage.getItem(SS_CHAT_KEY)
      if (raw) setMessages((JSON.parse(raw) as ChatMessage[]).map(normalizeStoredMessage))
      const yaml = sessionStorage.getItem(SS_YAML_KEY)
      if (yaml) {
        setProposedYaml(yaml)
        setPlaceholders(extractYamlPlaceholders(yaml))
        setSecretValues({})
      }
      const backup = sessionStorage.getItem(SS_BACKUP_KEY)
      if (backup) setBackupPath(backup)
    } catch { /* ignore */ }
  }, [])
  const [applying, setApplying] = useState(false)
  const [reverting, setReverting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [yamlStreaming, setYamlStreaming] = useState(false)
  const [streamingYaml, setStreamingYaml] = useState<string>("")
  const [applySuccess, setApplySuccess] = useState(false)
  const [revealSecrets, setRevealSecrets] = useState(false)

  // Typewriter queue
  const typewriterQueueRef = useRef<string[]>([])
  const typewriterIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const assistantContentRef = useRef<string>("")

  const chatScrollRef = useRef<HTMLDivElement>(null)
  const chatInputRef = useRef<HTMLInputElement>(null)

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  // Persist messages to sessionStorage on change
  useEffect(() => {
    try {
      const persisted = messages.map(normalizeStoredMessage)
      sessionStorage.setItem(SS_CHAT_KEY, JSON.stringify(persisted))
    } catch {}
  }, [messages])

  // Persist proposedYaml
  useEffect(() => {
    try {
      if (proposedYaml) sessionStorage.setItem(SS_YAML_KEY, proposedYaml)
      else sessionStorage.removeItem(SS_YAML_KEY)
    } catch {}
  }, [proposedYaml])

  // Persist backupPath
  useEffect(() => {
    try {
      if (backupPath) sessionStorage.setItem(SS_BACKUP_KEY, backupPath)
      else sessionStorage.removeItem(SS_BACKUP_KEY)
    } catch {}
  }, [backupPath])

  // Load current config on mount (and when revealSecrets changes)
  useEffect(() => {
    const t = token()
    const url = revealSecrets
      ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
      : `${hubUrl}/api/settings/ai-config/current-config`
    fetch(url, {
      headers: { Authorization: `Bearer ${t}` },
    })
      .then(r => {
        if (!r.ok) throw new Error(`Failed to fetch current config: ${r.status}`)
        return r.text()
      })
      .then(text => setCurrentConfig(text))
      .catch(() => {})
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revealSecrets])

  // Load existing backup on mount (only if not already persisted)
  useEffect(() => {
    if (backupPath) return // already have one from sessionStorage
    const t = token()
    fetch(`${hubUrl}/api/settings/ai-config/backup`, {
      headers: { Authorization: `Bearer ${t}` },
    })
      .then(r => r.json())
      .then(d => { if (d.backup_path) setBackupPath(d.backup_path) })
      .catch(() => {})
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Auto-scroll chat to bottom when messages update
  useEffect(() => {
    if (chatScrollRef.current) {
      chatScrollRef.current.scrollTop = chatScrollRef.current.scrollHeight
    }
  }, [messages])

  // Typewriter: drain queue at ~20ms intervals
  const startTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) return
    typewriterIntervalRef.current = setInterval(() => {
      const queue = typewriterQueueRef.current
      if (queue.length === 0) return
      // Drain up to a few chars per tick for smooth ~60fps feel
      const chars = queue.splice(0, 3).join("")
      assistantContentRef.current += chars
      const current = stripYamlBlocks(assistantContentRef.current)
      setMessages(prev => {
        const msgs = [...prev]
        const last = msgs[msgs.length - 1]
        if (last?.role === "assistant" && last.streaming) {
          msgs[msgs.length - 1] = { role: "assistant", content: current + "\u258c", streaming: true }
        }
        return msgs
      })
    }, 20)
  }, [])

  const stopTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) {
      clearInterval(typewriterIntervalRef.current)
      typewriterIntervalRef.current = null
    }
  }, [])

  useEffect(() => () => { stopTypewriter() }, [stopTypewriter])

  // Drain remaining queue and finalize
  const finalizeTypewriter = useCallback((dropIfEmpty = false) => {
    stopTypewriter()
    // Flush remaining queue synchronously
    const remaining = typewriterQueueRef.current.join("")
    typewriterQueueRef.current = []
    assistantContentRef.current += remaining
    const finalContent = assistantContentRef.current
    const visibleFinalContent = stripYamlBlocks(finalContent)
    setMessages(prev => {
      const msgs = [...prev]
      const last = msgs[msgs.length - 1]
      if (last?.role === "assistant" && last.streaming) {
        if (dropIfEmpty && visibleFinalContent.trim() === "") return msgs.slice(0, -1)
        msgs[msgs.length - 1] = { role: "assistant", content: finalContent, streaming: false }
      }
      return msgs
    })
    assistantContentRef.current = ""
  }, [stopTypewriter])

  const sendMessage = async () => {
    if (!input.trim() || loading) return
    const userMsg: ChatMessage = { role: "user", content: input.trim() }
    const historyForRequest = [...messages]

    // Reset typewriter state
    stopTypewriter()
    typewriterQueueRef.current = []
    assistantContentRef.current = ""

    setMessages(prev => [...prev, userMsg])
    setInput("")
    setLoading(true)
    setError(null)
    setProposedYaml(null)
    setPlaceholders([])
    setSecretValues({})
    setYamlStreaming(false)
    setStreamingYaml("")

    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/stream`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ message: userMsg.content, history: historyForRequest }),
      })
      if (!res.ok) throw new Error(await res.text())

      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let sseBuffer = ""

      // Add streaming placeholder
      setMessages(prev => [...prev, { role: "assistant", content: "", streaming: true }])
      let inYamlBlock = false
      let yamlFenceConsumed = false

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        sseBuffer += decoder.decode(value, { stream: true })

        const lines = sseBuffer.split("\n")
        sseBuffer = lines.pop() ?? ""

        for (const line of lines) {
          if (!line.startsWith("data: ")) continue
          let parsed: Record<string, unknown>
          try { parsed = JSON.parse(line.slice(6)) } catch { continue }

          if (parsed.type === "token") {
            const tokenText = parsed.content as string
            // Detect start of yaml block — redirect subsequent tokens to YAML panel
            const fullSoFar = assistantContentRef.current + typewriterQueueRef.current.join("") + tokenText
            if (!inYamlBlock && /```ya?ml/i.test(fullSoFar)) {
              const queued = typewriterQueueRef.current.join("")
              if (queued) {
                typewriterQueueRef.current = []
                assistantContentRef.current += queued
              }
              inYamlBlock = true
              yamlFenceConsumed = false
              setYamlStreaming(true)
              setStreamingYaml("")
            }
            if (inYamlBlock) {
              // Stream to YAML panel — strip the opening fence (may be mid-token or split across tokens)
              let content = tokenText
              if (!yamlFenceConsumed) {
                const fenceMatch = content.match(/```ya?ml\n?/i)
                if (fenceMatch && fenceMatch.index !== undefined) {
                  // Fence is in this token — drop everything up to and including the fence
                  content = content.slice(fenceMatch.index + fenceMatch[0].length)
                  yamlFenceConsumed = true
                } else {
                  // Fence was the entire previous token — mark consumed and pass content through
                  yamlFenceConsumed = true
                }
              }
              // Strip closing fence if it appears at the end
              const hadClosingFence = /```\s*$/.test(content)
              content = content.replace(/```\s*$/, "")
              if (content) setStreamingYaml(prev => prev + content)
              if (hadClosingFence) inYamlBlock = false
              // Still push to assistant content for stripping
              assistantContentRef.current += tokenText
            } else {
              // Push to typewriter queue instead of directly to state
              const chars = tokenText.split("")
              typewriterQueueRef.current.push(...chars)
              startTypewriter()
            }
            continue
          } else if (parsed.type === "proposed_yaml") {
            // YAML was already streamed live via token events — just finalize
            const yaml = parsed.yaml as string
            setYamlStreaming(false)
            setProposedYaml(yaml)
            setStreamingYaml("")
            inYamlBlock = false
          } else if (parsed.type === "placeholders") {
            setPlaceholders(parsed.items as string[])
            setSecretValues({})
          } else if (parsed.type === "error") {
            setError(parsed.content as string)
            finalizeTypewriter(true)
          } else if (parsed.type === "done") {
            // finalizeTypewriter() called in finally
          }
        }
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed")
    } finally {
      finalizeTypewriter(true)
      setYamlStreaming(false)
      setStreamingYaml("")
      setLoading(false)
      setTimeout(() => chatInputRef.current?.focus(), 0)
    }
  }

  const applyConfig = async () => {
    if (!proposedYaml) return
    setApplying(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/apply`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ proposed_yaml: proposedYaml, secrets: secretValues }),
      })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setBackupPath(data.backup_path)
      const cfgUrl = revealSecrets
        ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
        : `${hubUrl}/api/settings/ai-config/current-config`
      fetch(cfgUrl, {
        headers: { Authorization: `Bearer ${token()}` },
      }).then(r => r.text()).then(setCurrentConfig).catch(() => {})
      setProposedYaml(null)
      setPlaceholders([])
      setSecretValues({})
      setApplySuccess(true)
      setTimeout(() => setApplySuccess(false), 5000)
    } catch (e) {
      setError(e instanceof Error ? e.message : "Apply failed")
    } finally {
      setApplying(false)
    }
  }

  const revertConfig = async () => {
    if (!backupPath) return
    setReverting(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/revert`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ backup_path: backupPath }),
      })
      if (!res.ok) throw new Error(await res.text())
      setBackupPath(null)
      setApplySuccess(false)
      const cfgUrl = revealSecrets
        ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
        : `${hubUrl}/api/settings/ai-config/current-config`
      fetch(cfgUrl, {
        headers: { Authorization: `Bearer ${token()}` },
      }).then(r => r.text()).then(setCurrentConfig).catch(() => {})
    } catch (e) {
      setError(e instanceof Error ? e.message : "Revert failed")
    } finally {
      setReverting(false)
    }
  }

  const allPlaceholdersFilled = placeholders.every(p => secretValues[p]?.trim())
  const displayedYaml = yamlStreaming ? streamingYaml : (proposedYaml ?? currentConfig)
  const yamlLabel = yamlStreaming ? "Generating config…" : (proposedYaml ? "Proposed config" : "Current config")

  return (
    <div className="flex flex-col" style={{ height: "calc(100vh - 8rem)" }}>
      {/* Header */}
      <div className="px-8 pt-6 pb-3 flex-none flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold mb-0.5">Configure with AI</h2>
          <p className="text-sm text-muted-foreground">
            Describe changes in plain English. The AI will propose a hub.yaml update for you to review and apply.
          </p>
        </div>
        {messages.length > 0 && (
          <Button
            variant="ghost"
            size="sm"
            className="text-muted-foreground shrink-0"
            onClick={() => {
              setMessages([])
              setProposedYaml(null)
              setPlaceholders([])
              setSecretValues({})
              setError(null)
              setApplySuccess(false)
              setYamlStreaming(false)
              setStreamingYaml("")
              sessionStorage.removeItem("ai-config-chat-history")
              sessionStorage.removeItem("ai-config-proposed-yaml")
              sessionStorage.removeItem("ai-config-backup-path")
              setBackupPath(null)
              setTimeout(() => chatInputRef.current?.focus(), 0)
            }}
          >
            <RotateCcw className="size-3.5 mr-1.5" />
            Start over
          </Button>
        )}
      </div>

      {/* Status bar */}
      {(error || applySuccess || (backupPath && !applySuccess)) && (
        <div className="px-8 flex-none mb-2">
          {error && (
            <p className="text-sm text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>
          )}
          {applySuccess && (
            <div className="flex items-center gap-3">
              <p className="text-sm text-green-600">&check; Config applied.</p>
              {backupPath && (
                <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
                  <RotateCcw className="size-3.5 mr-1" />
                  {reverting ? "Reverting\u2026" : "Revert"}
                </Button>
              )}
            </div>
          )}
          {backupPath && !applySuccess && (
            <div className="flex items-center gap-3">
              <p className="text-xs text-muted-foreground">Previous backup available.</p>
              <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
                <RotateCcw className="size-3.5 mr-1" />
                {reverting ? "Reverting\u2026" : "Revert to backup"}
              </Button>
            </div>
          )}
        </div>
      )}

      {/* Two-column body — fills remaining height */}
      <div className="flex flex-1 min-h-0 px-8 pb-6 gap-4">

        {/* Left: chat */}
        <div className="flex flex-col flex-1 min-w-0 min-h-0 border border-border rounded-lg bg-muted/10 overflow-hidden">
          {/* Scrollable message history */}
          <div ref={chatScrollRef} className="flex-1 overflow-y-auto p-4 space-y-3">
            {messages.length === 0 && (
              <p className="text-sm text-muted-foreground text-center py-8">
                Describe the config change you&apos;d like to make.
              </p>
            )}
            {messages.map((m, i) => (
              <div key={i} className={cn("flex", m.role === "user" ? "justify-end" : "justify-start")}>
                <div
                  className={cn(
                    "max-w-[90%] rounded-xl px-3 py-2 text-sm break-words",
                    m.role === "user"
                      ? "bg-primary text-primary-foreground whitespace-pre-wrap"
                      : "bg-muted text-foreground"
                  )}
                >
                  {m.role === "assistant"
                    ? <span>{renderMarkdown(stripYamlBlocks(m.content.replace(/\u258c$/, "")))}{m.streaming && <span className="animate-pulse">&#x258c;</span>}</span>
                    : m.content
                  }
                </div>
              </div>
            ))}
            {loading && (messages.length === 0 || messages[messages.length - 1]?.role === "user") && (
              <div className="flex justify-start">
                <div className="bg-muted rounded-xl px-3 py-2 text-sm text-muted-foreground">
                  <span className="inline-flex gap-1">
                    <span className="animate-bounce" style={{ animationDelay: "0ms" }}>&middot;</span>
                    <span className="animate-bounce" style={{ animationDelay: "150ms" }}>&middot;</span>
                    <span className="animate-bounce" style={{ animationDelay: "300ms" }}>&middot;</span>
                  </span>
                </div>
              </div>
            )}
          </div>

          {/* Input pinned to bottom */}
          <div className="flex-none border-t border-border p-3 flex gap-2">
            <Input
              ref={chatInputRef}
              placeholder="e.g. Add a Linear integration for workspace acme"
              value={input}
              onChange={e => setInput(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendMessage() } }}
              disabled={loading}
              className="flex-1 text-sm"
              autoFocus
            />
            <Button size="icon" onClick={sendMessage} disabled={loading || !input.trim()}>
              <Send className="size-4" />
            </Button>
          </div>
        </div>

        {/* Right: config panel */}
        <div className="flex flex-col min-h-0 gap-3 flex-1 min-w-0">
          {/* Label + secret toggle */}
          <div className="flex-none flex items-center justify-between gap-2">
            <div className="flex items-center gap-2">
              <span className={cn(
                "text-xs font-medium uppercase tracking-wide px-2 py-0.5 rounded",
                yamlStreaming
                  ? "bg-blue-500/15 text-blue-500 dark:text-blue-400"
                  : proposedYaml
                    ? "bg-amber-500/15 text-amber-600 dark:text-amber-400"
                    : "bg-muted text-muted-foreground"
              )}>
                {yamlLabel}
              </span>
              {yamlStreaming && (
                <span className="flex gap-0.5 items-center">
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:0ms]" />
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:150ms]" />
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:300ms]" />
                </span>
              )}
            </div>
            <div className="flex items-center gap-2">
              {revealSecrets && (
                <span className="text-xs text-amber-500 font-medium">Secrets visible</span>
              )}
              <Button
                size="icon"
                variant="ghost"
                className="size-6"
                title={revealSecrets ? "Hide secrets" : "Reveal secrets"}
                onClick={() => setRevealSecrets(v => !v)}
              >
                {revealSecrets ? <Eye className="size-3.5" /> : <EyeOff className="size-3.5" />}
              </Button>
            </div>
          </div>

          {/* YAML display — fills available height, scrollable */}
          <div className={cn(
            "flex-1 min-h-0 border rounded-lg overflow-hidden bg-[#0d1117] relative transition-colors duration-300",
            yamlStreaming ? "border-blue-500/50" : "border-border"
          )}>
            {yamlStreaming
              ? <pre className="h-full overflow-auto p-3 text-xs font-mono leading-relaxed text-gray-300 whitespace-pre">{streamingYaml}<span className="animate-pulse text-amber-400">&#x258c;</span></pre>
              : displayedYaml
                ? <YamlHighlight code={displayedYaml} />
                : <p className="p-3 text-xs text-muted-foreground">Loading…</p>
            }
          </div>

          {/* Placeholder secret inputs */}
          {proposedYaml && placeholders.length > 0 && (
            <div className="flex-none border border-border rounded-lg p-3 space-y-2 bg-background">
              <p className="text-xs font-medium text-muted-foreground">Fill in secrets</p>
              {placeholders.map(ph => (
                <div key={ph}>
                  <label className="text-xs text-muted-foreground font-mono mb-1 block">{ph}</label>
                  <Input
                    type="password"
                    placeholder={`Value for ${ph}`}
                    value={secretValues[ph] || ""}
                    onChange={e => setSecretValues(prev => ({ ...prev, [ph]: e.target.value }))}
                    className="font-mono text-xs h-7"
                  />
                </div>
              ))}
            </div>
          )}

          {/* Apply / Discard buttons */}
          {proposedYaml && (
            <div className="flex-none flex gap-2">
              <Button
                onClick={applyConfig}
                disabled={applying || !allPlaceholdersFilled}
                className="flex-1"
              >
                {applying ? "Applying\u2026" : "Apply"}
              </Button>
              <Button
                variant="outline"
                disabled={applying}
                onClick={() => {
                  setProposedYaml(null)
                  setPlaceholders([])
                  setSecretValues({})
                }}
              >
                Discard
              </Button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}


function MCPServersSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => Promise<boolean>; saving: boolean }) {
  const mcps = settings.mcpServers || []
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [editIdx, setEditIdx] = useState<number | null>(null)

  // Form state
  const [formName, setFormName] = useState("")
  const [formSource, setFormSource] = useState<"npx" | "uvx" | "smithery" | "docker" | "sse">("npx")
  const [formPackage, setFormPackage] = useState("")
  const [formImage, setFormImage] = useState("")
  const [formURL, setFormURL] = useState("")
  const [formEnabled, setFormEnabled] = useState(true)
  const [formCommand, setFormCommand] = useState("")
  const [formConfig, setFormConfig] = useState<Record<string, string>>({})
  const [formSecrets, setFormSecrets] = useState<Record<string, string>>({})
  const [configKey, setConfigKey] = useState("")
  const [configValue, setConfigValue] = useState("")
  const [secretEnvVar, setSecretEnvVar] = useState("")
  const [secretRef, setSecretRef] = useState("")

  const resetForm = () => {
    setFormName(""); setFormSource("npx"); setFormPackage(""); setFormImage(""); setFormURL("");
    setFormEnabled(true); setFormCommand(""); setFormConfig({}); setFormSecrets({});
    setConfigKey(""); setConfigValue(""); setSecretEnvVar(""); setSecretRef(""); setEditIdx(null)
  }

  const openAdd = () => { resetForm(); setModalMode("add"); setShowModal(true) }
  const openEdit = (i: number) => {
    const m = mcps[i]
    setFormName(m.name)
    setFormSource(m.source as typeof formSource)
    setFormPackage(m.package || "")
    setFormImage(m.image || "")
    setFormURL(m.url || "")
    setFormEnabled(m.enabled)
    setFormCommand(m.command?.join(" ") || "")
    setFormConfig(m.config || {})
    // secrets is array of env var names in the view; we need to reconstruct from... wait,
    // the view has secrets as string[] (env var names only). We can't edit secret refs
    // in edit mode without the actual mapping. For now, start empty.
    setFormSecrets({})
    setEditIdx(i)
    setModalMode("edit")
    setShowModal(true)
  }

  const needsPackage = formSource === "npx" || formSource === "uvx" || formSource === "smithery"
  const needsImage = formSource === "docker"
  const needsURL = formSource === "sse"

  async function doSave() {
    const mcp = {
      name: formName.trim(),
      source: formSource,
      package: needsPackage ? formPackage.trim() || undefined : undefined,
      image: needsImage ? formImage.trim() || undefined : undefined,
      url: needsURL ? formURL.trim() || undefined : undefined,
      enabled: formEnabled,
      command: formCommand.trim() ? formCommand.trim().split(/\s+/).filter(Boolean) : undefined,
      config: Object.keys(formConfig).length > 0 ? formConfig : undefined,
      secrets: Object.keys(formSecrets).length > 0 ? formSecrets : undefined,
    }

    let ok = false
    if (modalMode === "add") {
      const updated = [...mcps.filter(m => m.name !== mcp.name), mcp]
      ok = await onSave({ mcpServers: updated })
    } else if (editIdx !== null) {
      const existing = mcps[editIdx]
      const patch: Record<string, unknown> = { name: existing.name }
      if (formName.trim() !== existing.name) patch.newName = formName.trim()
      if (formSource !== existing.source) patch.source = formSource
      if (needsPackage && formPackage.trim() !== (existing.package || "")) patch.package = formPackage.trim()
      if (needsImage && formImage.trim() !== (existing.image || "")) patch.image = formImage.trim()
      if (needsURL && formURL.trim() !== (existing.url || "")) patch.url = formURL.trim()
      if (formEnabled !== existing.enabled) patch.enabled = formEnabled
      if (formCommand.trim() !== (existing.command?.join(" ") || "")) patch.command = formCommand.trim().split(/\s+/).filter(Boolean)
      if (Object.keys(formConfig).length > 0) patch.config = formConfig
      if (Object.keys(formSecrets).length > 0) patch.secrets = formSecrets
      ok = await onSave({ mcpServers: [patch] })
    }
    if (ok) {
      setShowModal(false)
      resetForm()
    }
  }

  async function doRemove(i: number) {
    const ok = await onSave({ mcpServers: [{ name: mcps[i].name, delete: true }] })
    if (ok) {
      setShowModal(false)
      resetForm()
    }
  }

  async function doToggle(name: string) {
    await onSave({ mcpServers: [{ name, enabled: !mcps.find(m => m.name === name)?.enabled }] })
  }

  const enabledCount = mcps.filter(m => m.enabled).length

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">MCP Servers</h2>
        <p className="text-sm text-muted-foreground mb-4">
          Model Context Protocol servers add tools to your claws. Configure them here and reference them in workflows.
        </p>
        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {mcps.length} server{mcps.length !== 1 ? "s" : ""} configured
          </span>
          {enabledCount > 0 && (
            <span className="text-xs bg-green-500/10 text-green-400 border border-green-500/20 px-2 py-1 rounded font-medium">
              {enabledCount} enabled
            </span>
          )}
        </div>
      </div>

      {mcps.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center border border-border rounded-lg">
          No MCP servers configured.
        </p>
      ) : (
        <div className="space-y-2">
          {mcps.map((mcp, i) => (
            <div
              key={mcp.name}
              onClick={() => openEdit(i)}
              className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
            >
              <div className="flex items-center gap-3">
                <button
                  onClick={(e) => { e.stopPropagation(); doToggle(mcp.name) }}
                  className={cn(
                    "relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full transition-colors duration-200",
                    mcp.enabled
                      ? "bg-green-600 border-2 border-transparent"
                      : "bg-transparent border-2 border-muted-foreground/40"
                  )}
                >
                  <span className={cn(
                    "pointer-events-none inline-block size-4 rounded-full shadow-sm transform transition-transform duration-200",
                    mcp.enabled ? "bg-white translate-x-4" : "bg-muted-foreground/50 translate-x-0"
                  )} />
                </button>
                <div>
                  <div className="flex items-center gap-2">
                    <code className="text-sm font-mono font-medium">{mcp.name}</code>
                    <span className="text-xs text-muted-foreground capitalize">{mcp.source}</span>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {[
                      mcp.package && `package: ${mcp.package}`,
                      mcp.image && `image: ${mcp.image}`,
                      mcp.url && `url: ${mcp.url}`,
                      mcp.secrets && mcp.secrets.length > 0 && `${mcp.secrets.length} secret(s)`,
                    ].filter(Boolean).join(" · ")}
                  </p>
                </div>
              </div>
              <span className="text-muted-foreground text-lg">⋯</span>
            </div>
          ))}
        </div>
      )}

      <Button onClick={openAdd} className="gap-2">
        <span className="text-sm">+</span> Add MCP Server
      </Button>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetForm() }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">{modalMode === "add" ? "Add MCP Server" : `Edit ${formName}`}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{modalMode === "add" ? "Add MCP Server" : `Edit ${formName}`}</h3>
          </div>

          <div className="p-5 space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                  <Input
                    placeholder="e.g. github"
                    value={formName}
                    onChange={e => setFormName(e.target.value)}
                    className="font-mono text-sm h-8"
                    disabled={modalMode === "edit"}
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Source</label>
                  <select
                    value={formSource}
                    onChange={e => setFormSource(e.target.value as typeof formSource)}
                    className="w-full h-8 text-sm rounded-md border border-input bg-background px-3"
                  >
                    <option value="npx">npx</option>
                    <option value="uvx">uvx</option>
                    <option value="smithery">smithery</option>
                    <option value="docker">docker</option>
                    <option value="sse">sse</option>
                  </select>
                </div>
              </div>

              {needsPackage && (
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Package</label>
                  <Input placeholder="e.g. @modelcontextprotocol/server-github" value={formPackage} onChange={e => setFormPackage(e.target.value)} className="font-mono text-sm h-8" />
                </div>
              )}
              {needsImage && (
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Image</label>
                  <Input placeholder="e.g. mcp/postgres" value={formImage} onChange={e => setFormImage(e.target.value)} className="font-mono text-sm h-8" />
                </div>
              )}
              {needsURL && (
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">URL</label>
                  <Input placeholder="e.g. https://mcp.example.com/sse" value={formURL} onChange={e => setFormURL(e.target.value)} className="font-mono text-sm h-8" />
                </div>
              )}

              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Command override (optional)</label>
                <Input placeholder="e.g. npx -y @scope/package --flag" value={formCommand} onChange={e => setFormCommand(e.target.value)} className="font-mono text-sm h-8" />
              </div>

              <label className="flex items-center gap-2 text-sm cursor-pointer">
                <input type="checkbox" checked={formEnabled} onChange={e => setFormEnabled(e.target.checked)} />
                Enabled
              </label>

              {/* Config vars */}
              <div className="space-y-2">
                <label className="text-xs text-muted-foreground block">Config variables</label>
                {Object.entries(formConfig).map(([k, v]) => (
                  <div key={k} className="flex items-center gap-2">
                    <code className="text-xs font-mono">{k}</code>
                    <span className="text-xs text-muted-foreground">= {v}</span>
                    <Button size="sm" variant="ghost" className="h-6 px-2" onClick={() => {
                      const { [k]: _, ...rest } = formConfig
                      setFormConfig(rest)
                    }}>Remove</Button>
                  </div>
                ))}
                <div className="flex gap-2">
                  <Input placeholder="Key" value={configKey} onChange={e => setConfigKey(e.target.value)} className="font-mono text-xs h-7 flex-1" />
                  <Input placeholder="Value" value={configValue} onChange={e => setConfigValue(e.target.value)} className="font-mono text-xs h-7 flex-1" />
                  <Button size="sm" variant="outline" className="h-7" onClick={() => {
                    if (!configKey.trim()) return
                    setFormConfig({ ...formConfig, [configKey.trim()]: configValue })
                    setConfigKey(""); setConfigValue("")
                  }}>Add</Button>
                </div>
              </div>

              {/* Secret refs */}
              <div className="space-y-2">
                <label className="text-xs text-muted-foreground block">Secret references</label>
                {Object.entries(formSecrets).map(([envVar, ref]) => (
                  <div key={envVar} className="flex items-center gap-2">
                    <code className="text-xs font-mono">{envVar}</code>
                    <span className="text-xs text-muted-foreground">→ secret: {ref}</span>
                    <Button size="sm" variant="ghost" className="h-6 px-2" onClick={() => {
                      const { [envVar]: _, ...rest } = formSecrets
                      setFormSecrets(rest)
                    }}>Remove</Button>
                  </div>
                ))}
                <div className="flex gap-2">
                  <Input placeholder="Env var (e.g. GITHUB_TOKEN)" value={secretEnvVar} onChange={e => setSecretEnvVar(e.target.value)} className="font-mono text-xs h-7 flex-1" />
                  <select
                    value={secretRef}
                    onChange={e => setSecretRef(e.target.value)}
                    className="h-7 text-xs rounded-md border border-input bg-background px-2 flex-1"
                  >
                    <option value="">Select secret…</option>
                    {(settings.secrets || []).map(s => (
                      <option key={s} value={s}>{s}</option>
                    ))}
                  </select>
                  <Button size="sm" variant="outline" className="h-7" onClick={() => {
                    if (!secretEnvVar.trim() || !secretRef) return
                    setFormSecrets({ ...formSecrets, [secretEnvVar.trim()]: secretRef })
                    setSecretEnvVar(""); setSecretRef("")
                  }}>Add</Button>
                </div>
              </div>
            </div>

            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => doRemove(editIdx)}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetForm() }}>Cancel</Button>
                <Button
                  size="sm"
                  disabled={saving || !formName.trim() || (needsPackage && !formPackage.trim()) || (needsImage && !formImage.trim()) || (needsURL && !formURL.trim())}
                  onClick={doSave}
                >
                  {modalMode === "add" ? "Add MCP Server" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// linkifyText converts URLs in text into clickable <a> elements.
function linkifyText(text: string): React.ReactNode {
  const urlRegex = /(https?:\/\/[^\s]+)/g
  const parts = text.split(urlRegex)
  return parts.map((part, i) => {
    if (part.match(urlRegex)) {
      return (
        <a
          key={i}
          href={part}
          target="_blank"
          rel="noopener noreferrer"
          className="text-primary hover:underline"
        >
          {part}
        </a>
      )
    }
    return part
  })
}

const TIME_RANGES = [
  { id: "just-now", label: "Just now", desc: "last 5 min" },
  { id: "15min", label: "Last 15 min", desc: "last 15 min" },
  { id: "1hour", label: "Past hour", desc: "last 1 hour" },
  { id: "4hours", label: "~4 hours ago", desc: "last 4 hours" },
  { id: "today", label: "Today", desc: "since midnight" },
]

function TroubleshootSection() {
  const [problem, setProblem] = useState("")
  const [timeRange, setTimeRange] = useState<string | null>(null)
  const [stillHappening, setStillHappening] = useState<boolean | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [diagnosis, setDiagnosis] = useState<string | null>(null)
  const [submitted, setSubmitted] = useState(false)
  const [streamingText, setStreamingText] = useState("")

  const typewriterQueueRef = useRef<string[]>([])
  const typewriterIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const contentRef = useRef<string>("")
  const diagnosisScrollRef = useRef<HTMLDivElement>(null)

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const canSubmit = problem.trim() !== "" && timeRange !== null && !loading

  const startTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) return
    typewriterIntervalRef.current = setInterval(() => {
      const queue = typewriterQueueRef.current
      if (queue.length === 0) return
      const chars = queue.splice(0, 3).join("")
      contentRef.current += chars
      setStreamingText(contentRef.current + "▌")
      if (diagnosisScrollRef.current) {
        diagnosisScrollRef.current.scrollTop = diagnosisScrollRef.current.scrollHeight
      }
    }, 20)
  }, [])

  const stopTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) {
      clearInterval(typewriterIntervalRef.current)
      typewriterIntervalRef.current = null
    }
  }, [])

  useEffect(() => () => { stopTypewriter() }, [stopTypewriter])

  const submit = async () => {
    if (!canSubmit) return
    setLoading(true)
    setError(null)
    setDiagnosis(null)
    setSubmitted(true)
    setStreamingText("")
    contentRef.current = ""
    typewriterQueueRef.current = []

    try {
      const body: Record<string, unknown> = { problem: problem.trim(), time_range: timeRange }
      if (stillHappening !== null) body.still_happening = stillHappening

      const res = await fetch(`${hubUrl}/api/troubleshoot/stream`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
      if (!res.ok) throw new Error(await res.text())

      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let sseBuffer = ""

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        sseBuffer += decoder.decode(value, { stream: true })
        const lines = sseBuffer.split("\n")
        sseBuffer = lines.pop() ?? ""
        for (const line of lines) {
          if (!line.startsWith("data: ")) continue
          let parsed: Record<string, unknown>
          try { parsed = JSON.parse(line.slice(6)) } catch { continue }
          if (parsed.type === "token") {
            typewriterQueueRef.current.push(...(parsed.content as string).split(""))
            startTypewriter()
          } else if (parsed.type === "error") {
            setError(parsed.content as string)
          }
        }
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed")
    } finally {
      stopTypewriter()
      const remaining = typewriterQueueRef.current.join("")
      typewriterQueueRef.current = []
      contentRef.current += remaining
      setStreamingText("")
      setDiagnosis(contentRef.current)
      contentRef.current = ""
      setLoading(false)
    }
  }

  const reset = () => {
    setProblem("")
    setTimeRange(null)
    setStillHappening(null)
    setLoading(false)
    setError(null)
    setDiagnosis(null)
    setSubmitted(false)
    setStreamingText("")
    stopTypewriter()
    typewriterQueueRef.current = []
    contentRef.current = ""
  }

  const displayText = diagnosis || streamingText

  return (
    <div className="flex flex-col" style={{ height: "calc(100vh - 8rem)" }}>
      <div className="px-8 pt-6 pb-3 flex-none flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold mb-0.5">Troubleshoot</h2>
          <p className="text-sm text-muted-foreground">
            Describe what&apos;s not working. The AI will analyze your logs, config, and source code.
          </p>
        </div>
        {submitted && (
          <Button size="sm" variant="outline" onClick={reset} disabled={loading} className="shrink-0 mt-0.5">
            Start over
          </Button>
        )}
      </div>

      {!submitted ? (
        <div className="flex-1 overflow-y-auto px-8 pb-8">
          <div className="space-y-5 max-w-lg">
            <div>
              <label className="text-sm font-medium mb-2 block">What&apos;s happening?</label>
              <textarea
                className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring min-h-[100px] resize-none"
                placeholder="e.g. Workflow webhook is firing but the machine isn't being bootstrapped..."
                value={problem}
                onChange={e => setProblem(e.target.value)}
                disabled={loading}
              />
            </div>

            <div>
              <label className="text-sm font-medium mb-2 block">When did this happen?</label>
              <div className="flex flex-wrap gap-2">
                {TIME_RANGES.map(({ id, label }) => (
                  <Button
                    key={id}
                    size="sm"
                    variant={timeRange === id ? "default" : "outline"}
                    onClick={() => setTimeRange(id)}
                    disabled={loading}
                  >
                    {label}
                  </Button>
                ))}
              </div>
            </div>

            <div>
              <label className="text-sm font-medium mb-2 block">Still happening?</label>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant={stillHappening === true ? "default" : "outline"}
                  onClick={() => setStillHappening(stillHappening === true ? null : true)}
                  disabled={loading}
                >
                  Yes
                </Button>
                <Button
                  size="sm"
                  variant={stillHappening === false ? "default" : "outline"}
                  onClick={() => setStillHappening(stillHappening === false ? null : false)}
                  disabled={loading}
                >
                  No
                </Button>
              </div>
            </div>

            <Button onClick={submit} disabled={!canSubmit}>
              Diagnose
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex-1 min-h-0 flex flex-col px-8 pb-8 gap-4 overflow-hidden">
          <div className="rounded-lg border border-border bg-secondary/30 p-4 flex-none">
            <p className="text-xs text-muted-foreground mb-1">Your report</p>
            <p className="text-sm">{problem.trim()}</p>
            <div className="flex gap-3 mt-2">
              <span className="text-xs text-muted-foreground">{TIME_RANGES.find(t => t.id === timeRange)?.label}</span>
              {stillHappening !== null && (
                <span className="text-xs text-muted-foreground">• {stillHappening ? "Still happening" : "Resolved"}</span>
              )}
            </div>
          </div>

          {error && (
            <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-3 text-sm text-red-500 flex-none">
              {error}
            </div>
          )}

          {loading && !streamingText && (
            <div className="flex items-center gap-2 text-sm text-muted-foreground flex-none">
              <RotateCcw className="size-3.5 animate-spin" />
              Gathering logs and analyzing…
            </div>
          )}

          {displayText && (
            <div
              ref={diagnosisScrollRef}
              className="flex-1 overflow-y-auto rounded-lg border border-border p-4 text-sm whitespace-pre-wrap font-mono leading-relaxed bg-background"
            >
              {displayText}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function DoctorSection() {
  const [report, setReport] = useState<any>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showPassed, setShowPassed] = useState(false)

  const load = useCallback(async (refresh = false) => {
    setLoading(true)
    setError(null)
    try {
      const hubUrl = getHubUrl()
      const token = getAuthToken() || ""
      const res = await fetch(`${hubUrl}/api/doctor${refresh ? "?refresh=true" : ""}`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!res.ok) throw new Error(await res.text())
      setReport(await res.json())
    } catch (e: any) {
      setError(e.message || "Failed to load diagnostics")
    }
    setLoading(false)
  }, [])

  useEffect(() => { load() }, [load])

  const failedChecks = report?.checks?.filter((c: any) => !c.ok) || []
  const passedChecks = report?.checks?.filter((c: any) => c.ok) || []
  const visibleChecks = showPassed ? report?.checks || [] : failedChecks
  const allPassing = failedChecks.length === 0 && report?.checks?.length > 0

  const severityIcon = (s: string) => {
    switch (s) {
      case "critical": return <AlertTriangle className="size-4 text-red-500" />
      case "warning": return <AlertTriangle className="size-4 text-amber-500" />
      default: return <CheckCircle2 className="size-4 text-blue-400" />
    }
  }

  const severityBadge = (s: string) => {
    const classes: Record<string, string> = {
      critical: "bg-red-500/10 text-red-500 border-red-500/20",
      warning: "bg-amber-500/10 text-amber-500 border-amber-500/20",
      info: "bg-blue-400/10 text-blue-400 border-blue-400/20",
    }
    return (
      <span className={cn("text-[10px] uppercase tracking-wider font-semibold px-2 py-0.5 rounded border", classes[s] || classes.info)}>
        {s}
      </span>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold mb-1">Doctor</h2>
          <p className="text-sm text-muted-foreground">
            Diagnose hub configuration issues and get actionable fixes.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {report?.cachedAt && (
            <span className="text-xs text-muted-foreground">
              Cached {new Date(report.cachedAt).toLocaleTimeString()}
            </span>
          )}
          {report && passedChecks.length > 0 && (
            <Button
              size="sm"
              variant={showPassed ? "default" : "outline"}
              onClick={() => setShowPassed(!showPassed)}
            >
              <CheckCircle2 className="size-3.5 mr-1.5" />
              {showPassed ? "Hide passed" : `Show passed (${passedChecks.length})`}
            </Button>
          )}
        </div>
      </div>

      {error && (
        <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-4 text-sm text-red-500">
          {error}
        </div>
      )}

      {report && (
        <>
          <div className="grid grid-cols-4 gap-3">
            <div className="rounded-lg border border-border p-3 text-center">
              <p className="text-2xl font-bold">{report.summary.total}</p>
              <p className="text-xs text-muted-foreground mt-1">Checks</p>
            </div>
            <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-red-500">{report.summary.critical}</p>
              <p className="text-xs text-muted-foreground mt-1">Critical</p>
            </div>
            <div className="rounded-lg border border-amber-500/20 bg-amber-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-amber-500">{report.summary.warning}</p>
              <p className="text-xs text-muted-foreground mt-1">Warnings</p>
            </div>
            <div className="rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-emerald-500">{report.summary.passed}</p>
              <p className="text-xs text-muted-foreground mt-1">Passed</p>
            </div>
          </div>

          {visibleChecks.length === 0 ? (
            allPassing ? (
              <div className="rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-6 text-center">
                <CheckCircle2 className="size-8 text-emerald-500 mx-auto mb-2" />
                <p className="text-sm font-medium text-emerald-500">All checks passing</p>
                <p className="text-xs text-muted-foreground mt-1">
                  {report.summary.passed} check{report.summary.passed !== 1 ? "s" : ""} passed with no issues
                </p>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">No checks to display.</p>
            )
          ) : (
            <div className="space-y-3">
              {visibleChecks.map((check: any, i: number) => (
                <div
                  key={i}
                  className={cn(
                    "rounded-lg border p-4",
                    check.ok
                      ? "border-emerald-500/20 bg-emerald-500/5"
                      : check.severity === "critical"
                        ? "border-red-500/20 bg-red-500/5"
                        : check.severity === "warning"
                          ? "border-amber-500/20 bg-amber-500/5"
                          : "border-border"
                  )}
                >
                  <div className="flex items-start gap-3">
                    <div className="mt-0.5 shrink-0">
                      {check.ok ? (
                        <CheckCircle2 className="size-4 text-emerald-500" />
                      ) : (
                        severityIcon(check.severity)
                      )}
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
                          {check.category}
                        </span>
                        {!check.ok && severityBadge(check.severity)}
                      </div>
                      <p className="text-sm font-medium mt-1">{check.title}</p>
                      <p className="text-sm text-muted-foreground mt-0.5 whitespace-pre-wrap">{linkifyText(check.description)}</p>
                      {check.error && (
                        <p className="text-xs text-red-400 mt-1 font-mono">{check.error}</p>
                      )}
                      {check.fixAction && !check.ok && (
                        <div className="mt-3">
                          {check.fixAction.type === "navigate" ? (
                            <Button
                              size="sm"
                              variant="outline"
                              className="text-xs"
                              asChild
                            >
                              <Link href={check.fixAction.target}>
                                {check.fixAction.label}
                                <ArrowRight className="size-3 ml-1" />
                              </Link>
                            </Button>
                          ) : (
                            <Button
                              size="sm"
                              variant="outline"
                              className="text-xs"
                              disabled
                              title={`Action type "${check.fixAction.type}" not yet supported in UI`}
                            >
                              {check.fixAction.label}
                            </Button>
                          )}
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}
