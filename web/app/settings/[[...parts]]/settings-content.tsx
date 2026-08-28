"use client"

import { useParams, usePathname, useRouter } from "next/navigation"
import React, { useEffect, useState, useCallback, useRef } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Copy, Check, LayoutTemplate, Trash2, Lock, Sparkles, Send, RotateCcw, Eye, EyeOff, ExternalLink, AlertTriangle, X, CheckCircle2, Webhook, Stethoscope, ArrowRight, Wrench, GitBranch, ChevronDown, Bell, Clock } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import Link from "next/link"
import { VALID_SECTIONS, type Section } from "./sections"
import { fetchWorkspaces, updateWorkflowControls, type RepositoryAccess, type Workspace, type Workflow } from "@/lib/api"
import { useBranding } from "@/hooks/use-branding"
import { WorkflowName } from "@/components/workflow-name"
import { WorkflowRunsDialog } from "@/components/workflow-runs-dialog"

function isValidSection(s: string): s is Section {
  return VALID_SECTIONS.includes(s as Section)
}

const WORKSPACE_SECTIONS = new Set<Section>([
  "workspaces",
  "workflows",
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
  authProfile?: string
}

interface ModelAuthProfileView {
  name: string
  provider: string
  mode: string
  authenticated: boolean
  updatedAt?: string
}

interface ModelAuthLoginJob {
  id: string
  provider: string
  profile: string
  status: string
  url?: string
  code?: string
  output?: string
  error?: string
}

interface LLMModelOption {
  id: string
  name: string
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
  defaultOpenClawImage: string
  llmKeys: LLMKeyView[]
  modelOptions?: Record<string, LLMModelOption[]>
  modelAuthProfiles?: ModelAuthProfileView[]
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
    image?: string
    network?: string
    awsRegion?: string
    awsProfile?: string
    imageIdentifier?: string
    imageVersion?: string
    executionRoleArn?: string
    ingressNetworkConnectors?: string[]
    egressNetworkConnectors?: string[]
    idleMaxDurationSeconds?: number
    suspendedDurationSeconds?: number
    autoResume?: boolean
    maximumDurationSeconds?: number
    bridgePort?: number
    authTokenExpirationMinutes?: number
  }>
  github: GitHubAppView[]
  sshPublicKeys: string[]
  integrations?: {
    linear?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    shortcut?: Array<{ workspace: string; tokenSet: boolean }>
    githubIssues?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    jira?: Array<{ workspace: string; baseUrl?: string; username?: string; tokenSet: boolean; webhookSecretSet: boolean }>
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
  notifications?: NotificationsView | null
  lifecycleEventTypes?: string[]
  concurrencyGroups?: ConcurrencyGroup[]
  maxConcurrentClaws?: number
}

interface ConcurrencyGroup {
  name: string
  limit: number
}

// Outbound notification config, as returned by GET /api/settings. Notifier
// settings are provider-specific and inline next to the type, so they keep the
// snake_case wire names the hub reads them under. The lifecycle block is the
// redacted view, and every one of its fields round-trips under the same name
// the PATCH payload (types.LifecycleNotificationsConfig) reads it under.
interface NotifierView {
  type: string
  channel?: string
  token_secret?: string
  api_base?: string
  min_send_interval?: string
}

interface LifecycleRouteView {
  via: string
  // An empty (or absent) event list is an allow-all: the channel receives
  // every lifecycle alert type.
  events?: string[]
}

interface LifecycleEventToggles {
  agentStarted?: boolean
  prOpened?: boolean
  failures?: boolean
  agentIdle?: boolean
  stageStalled?: boolean
}

// One recurring report, as returned by GET /api/settings and read back by
// PATCH. Every field round-trips under the name the hub decodes it under
// (types.ScheduledNotificationConfig), and the hub resolves both defaults it
// carries before sending it here: `enabled` is always a real boolean, and
// `weekdays` is always an array — empty meaning every day.
interface ScheduledNotificationView {
  id: string
  report: string
  via: string[]
  at: string
  timezone?: string
  weekdays: string[]
  enabled: boolean
}

interface NotificationsView {
  notifiers?: Record<string, NotifierView>
  scheduled?: ScheduledNotificationView[]
  lifecycle?: {
    enabled: boolean
    // Legacy single-channel field, superseded by routes. The hub clears it as
    // soon as a patch carries routes.
    via?: string
    routes?: LifecycleRouteView[]
    pollInterval?: string
    idleAfter?: string
    stageProgressAfter?: string
    events?: LifecycleEventToggles
  }
}

// The outcome of one save. `persisted` says whether the hub accepted the PATCH;
// `message` is what to show, which is non-null both for a rejected PATCH and
// for an accepted one whose follow-up re-read failed.
type SaveOutcome = { persisted: boolean; message: string | null }

// What every screen says once the loaded settings can no longer be trusted. A
// reload is the only repair: the snapshot the patches are built from is stale,
// and this page has no other way to re-read it than the one that just failed.
const STALE_SETTINGS_MESSAGE = "Reload the page before editing again — until then every save is refused, because it would re-send the settings this screen last read and revert what the hub now holds."

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

async function startModelAuthLogin(provider: string, profile: string): Promise<ModelAuthLoginJob> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings/model-auth/login`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify({ provider, profile, mode: "device" }),
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

async function fetchModelAuthLogin(id: string): Promise<ModelAuthLoginJob> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings/model-auth/login/${encodeURIComponent(id)}`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
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
  // Latched when a save landed but its follow-up re-read did not: `settings`
  // then describes a hub that has already moved on, and every section builds
  // its next patch from that snapshot — the next save would re-send the values
  // this one replaced, recreating a channel the operator just deleted under a
  // green "Saved". A ref, not state: the gate has to hold for a save started
  // from the same render as the one that failed.
  const staleSettings = useRef(false)
  const [error, setError] = useState("")
  const [success, setSuccess] = useState("")
  const [version, setVersion] = useState("")
  const [hubPublicUrl, setHubPublicUrl] = useState("")
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [selectedWorkspace, setSelectedWorkspace] = useState("")
  const selectedWorkspaceLabel = selectedWorkspace || "No workspaces"
  const selectedWorkspaceInitial = selectedWorkspace ? selectedWorkspace.trim()[0].toUpperCase() : "-"

  // Rejects on failure: the re-fetch that follows a save is part of the save,
  // not bookkeeping after it (see runSave), so its caller has to see the error.
  const load = useCallback(() => fetchSettings().then((data) => setSettings(data)), [])

  useEffect(() => {
    load().catch((e) => setError(e instanceof Error ? e.message : "Failed to load"))
  }, [load])

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

  // runSave patches the settings and re-fetches them, returning null on success
  // or the message to show. The re-fetch is not optional bookkeeping: every
  // section builds its next patch from `settings`, so a swallowed reload
  // failure leaves the screen editing a snapshot the hub has already moved
  // past, and the next save re-sends those stale values — reverting whatever
  // this one just persisted. Report it as a failed save so the dialog that
  // triggered it stays open instead of closing on data it can no longer trust.
  //
  // `persisted` separates the two failures: the hub is unchanged only when the
  // PATCH itself was rejected. A caller that records what the hub now holds —
  // the notifier section's clamped-pause flag — has to write it either way, or
  // a re-read that failed after an accepted PATCH loses the record of a value
  // this screen wrote on its own initiative.
  async function runSave(patch: object): Promise<SaveOutcome> {
    if (staleSettings.current) {
      // Reporting it again is all this can do: the snapshot every patch is
      // built from is stale, so sending one would revert whatever the save
      // whose re-read failed had persisted.
      return { persisted: false, message: STALE_SETTINGS_MESSAGE }
    }
    setSaving(true)
    setError("")
    setSuccess("")
    try {
      try {
        await patchSettings(patch)
      } catch (e) {
        return { persisted: false, message: e instanceof Error ? e.message : "Save failed" }
      }
      try {
        await load()
      } catch (e) {
        const reason = e instanceof Error ? e.message : "the settings could not be re-read"
        staleSettings.current = true
        return {
          persisted: true,
          message: `Saved, but reloading the settings failed (${reason}). ${STALE_SETTINGS_MESSAGE}`,
        }
      }
      setSuccess("Saved")
      setTimeout(() => setSuccess(""), 2000)
      return { persisted: true, message: null }
    } finally {
      setSaving(false)
    }
  }

  async function save(patch: object): Promise<boolean> {
    const { message } = await runSave(patch)
    if (message) setError(message)
    return message === null
  }

  // save(), but handing the failure message back to the caller. The page-level
  // banner lives inside <main>, which sits behind a modal overlay — a section
  // that saves from its own dialog has to render the error there instead, or
  // the button looks dead.
  async function saveReportingError(patch: object): Promise<SaveOutcome> {
    const outcome = await runSave(patch)
    if (outcome.message) setError(outcome.message)
    return outcome
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
        { id: "notifier", label: "Notifier", icon: Bell },
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
        <main className={(section === "ai-config" || section === "troubleshoot") ? "flex-1 min-h-0 flex flex-col overflow-hidden" : "flex-1 overflow-y-auto p-8 max-w-4xl"}>
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
          {section === "secrets" && (
            <SecretsSection settings={settings} workspace={selectedWorkspace} />
          )}
          {settings && section === "mcp-servers" && (
            <MCPServersSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "notifier" && (
            <NotifierSection settings={settings} onSave={saveReportingError} saving={saving} />
          )}
          {section === "ai-config" && (
            <AIConfigSection />
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
  { value: "docker", label: "Local Docker", description: "Local Docker daemon provider for development and testing" },
  { value: "lambda-microvms", label: "AWS Lambda MicroVMs", description: "Serverless Firecracker MicroVM provider", alpha: true },
]

interface SandboxProviderView {
  name: string
  type: string
  label: string
  description: string
  alpha?: boolean
  configured: boolean
  apiUrl?: string
  apiKeySet?: boolean
  defaultSnapshot?: string
  tokenSet?: boolean
  defaultTtl?: string
  defaultInstanceType?: string
  image?: string
  network?: string
  imageIdentifier?: string
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
        alpha: opt?.alpha,
        configured: !!(p.tokenSet || p.apiKeySet || p.imageIdentifier || name === "exedev" || name === "docker"),
        apiUrl: p.apiUrl,
        apiKeySet: p.apiKeySet,
        defaultSnapshot: p.defaultSnapshot,
        tokenSet: p.tokenSet,
        defaultTtl: p.defaultTtl,
        defaultInstanceType: p.defaultInstanceType,
        image: p.image,
        network: p.network,
        imageIdentifier: p.imageIdentifier,
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
  const [formDockerImage, setFormDockerImage] = useState("")
  const [formDockerNetwork, setFormDockerNetwork] = useState("")
  const [formAwsRegion, setFormAwsRegion] = useState("")
  const [formAwsProfile, setFormAwsProfile] = useState("")
  const [formImageIdentifier, setFormImageIdentifier] = useState("")
  const [formImageVersion, setFormImageVersion] = useState("")
  const [formExecutionRoleArn, setFormExecutionRoleArn] = useState("")
  const [formIngressConnectors, setFormIngressConnectors] = useState("")
  const [formEgressConnectors, setFormEgressConnectors] = useState("")
  const [formIdleMaxDuration, setFormIdleMaxDuration] = useState("")
  const [formSuspendedDuration, setFormSuspendedDuration] = useState("")
  const [formAutoResume, setFormAutoResume] = useState(true)
  const [formMaximumDuration, setFormMaximumDuration] = useState("")
  const [formBridgePort, setFormBridgePort] = useState("")
  const [formAuthTokenExpiration, setFormAuthTokenExpiration] = useState("")

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
    setFormDockerImage("")
    setFormDockerNetwork("")
    setFormAwsRegion("")
    setFormAwsProfile("")
    setFormImageIdentifier("")
    setFormImageVersion("")
    setFormExecutionRoleArn("")
    setFormIngressConnectors("")
    setFormEgressConnectors("")
    setFormIdleMaxDuration("900")
    setFormSuspendedDuration("300")
    setFormAutoResume(true)
    setFormMaximumDuration("28800")
    setFormBridgePort("8080")
    setFormAuthTokenExpiration("30")
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
    setFormDockerImage(p?.image || "")
    setFormDockerNetwork(p?.network || "")
    setFormAwsRegion(p?.awsRegion || "")
    setFormAwsProfile(p?.awsProfile || "")
    setFormImageIdentifier(p?.imageIdentifier || "")
    setFormImageVersion(p?.imageVersion || "")
    setFormExecutionRoleArn(p?.executionRoleArn || "")
    setFormIngressConnectors((p?.ingressNetworkConnectors || []).join("\n"))
    setFormEgressConnectors((p?.egressNetworkConnectors || []).join("\n"))
    setFormIdleMaxDuration(p?.idleMaxDurationSeconds?.toString() || "900")
    setFormSuspendedDuration(p?.suspendedDurationSeconds?.toString() || "300")
    setFormAutoResume(p?.autoResume ?? true)
    setFormMaximumDuration(p?.maximumDurationSeconds?.toString() || "28800")
    setFormBridgePort(p?.bridgePort?.toString() || "8080")
    setFormAuthTokenExpiration(p?.authTokenExpirationMinutes?.toString() || "30")
    setEditName(name)
    setModalMode("edit")
    setShowModal(true)
  }

  const availableProviders = SANDBOX_PROVIDER_OPTIONS.filter(o => !providers[o.value])

  function splitConnectorList(value: string) {
    return value
      .split(/[\n,]/)
      .map(v => v.trim())
      .filter(Boolean)
  }

  function setPositiveInt(patch: Record<string, unknown>, key: string, value: string) {
    const parsed = parseInt(value, 10)
    if (value && !isNaN(parsed) && parsed > 0) patch[key] = parsed
  }

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
    } else if (formProvider === "docker") {
      patch.enabled = true
      patch.image = formDockerImage.trim()
      patch.network = formDockerNetwork.trim()
    } else if (formProvider === "lambda-microvms") {
      patch.enabled = true
      if (formAwsRegion) patch.awsRegion = formAwsRegion.trim()
      if (formAwsProfile) patch.awsProfile = formAwsProfile.trim()
      if (formImageIdentifier) patch.imageIdentifier = formImageIdentifier.trim()
      if (formImageVersion) patch.imageVersion = formImageVersion.trim()
      if (formExecutionRoleArn) patch.executionRoleArn = formExecutionRoleArn.trim()
      patch.ingressNetworkConnectors = splitConnectorList(formIngressConnectors)
      patch.egressNetworkConnectors = splitConnectorList(formEgressConnectors)
      setPositiveInt(patch, "idleMaxDurationSeconds", formIdleMaxDuration)
      setPositiveInt(patch, "suspendedDurationSeconds", formSuspendedDuration)
      patch.autoResume = formAutoResume
      setPositiveInt(patch, "maximumDurationSeconds", formMaximumDuration)
      setPositiveInt(patch, "bridgePort", formBridgePort)
      setPositiveInt(patch, "authTokenExpirationMinutes", formAuthTokenExpiration)
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
        <p className="text-sm text-muted-foreground mb-4">Configure VM providers for spawning agents.</p>

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
                    {p.alpha && (
                      <span className="text-xs bg-amber-500/20 text-amber-300 px-1.5 py-0.5 rounded">
                        Alpha
                      </span>
                    )}
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
                  {availableProviders.map(o => <option key={o.value} value={o.value}>{o.label}{o.alpha ? " (alpha)" : ""}</option>)}
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

            {formProvider === "docker" && (
              <>
                <div className="bg-muted/50 rounded-lg p-3 space-y-2">
                  <p className="text-xs font-medium">Local Docker provider</p>
                  <p className="text-xs text-muted-foreground">
                    Runs agent containers with the hub host&apos;s Docker daemon. If the hub runs in a container, mount the Docker socket so it can create sibling containers.
                  </p>
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Agent image</label>
                  <Input
                    value={formDockerImage}
                    onChange={e => setFormDockerImage(e.target.value)}
                    className="h-8 text-sm font-mono"
                    placeholder={settings.defaultOpenClawImage}
                  />
                  <p className="text-[11px] text-muted-foreground mt-1">
                    Leave blank to use the pinned OpenClaw image, {settings.defaultOpenClawImage}.
                  </p>
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Docker network</label>
                  <Input
                    value={formDockerNetwork}
                    onChange={e => setFormDockerNetwork(e.target.value)}
                    className="h-8 text-sm font-mono"
                    placeholder="elasticclaw-dev"
                  />
                  <p className="text-[11px] text-muted-foreground mt-1">
                    Optional. Use a network where agent containers can reach the hub.
                  </p>
                </div>
              </>
            )}

            {formProvider === "lambda-microvms" && (
              <>
                <div className="bg-amber-500/10 border border-amber-500/20 rounded-lg p-3">
                  <div className="flex items-center gap-2 mb-1">
                    <span className="text-xs bg-amber-500/20 text-amber-300 px-1.5 py-0.5 rounded">Alpha</span>
                    <p className="text-xs font-medium">AWS Lambda MicroVMs</p>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    Requires an image that starts the Elastic Claw bridge from the MicroVM run hook payload.
                  </p>
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Image identifier</label>
                  <Input
                    value={formImageIdentifier}
                    onChange={e => setFormImageIdentifier(e.target.value)}
                    className="h-8 text-sm font-mono"
                    placeholder="arn:aws:lambda:us-east-1:123456789012:microvm-image/elasticclaw"
                  />
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">AWS Region</label>
                    <Input value={formAwsRegion} onChange={e => setFormAwsRegion(e.target.value)} className="h-8 text-sm" placeholder="us-east-1" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">AWS Profile</label>
                    <Input value={formAwsProfile} onChange={e => setFormAwsProfile(e.target.value)} className="h-8 text-sm" placeholder="default" />
                  </div>
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Image version</label>
                  <Input value={formImageVersion} onChange={e => setFormImageVersion(e.target.value)} className="h-8 text-sm" placeholder="latest" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Execution role ARN</label>
                  <Input
                    value={formExecutionRoleArn}
                    onChange={e => setFormExecutionRoleArn(e.target.value)}
                    className="h-8 text-sm font-mono"
                    placeholder="arn:aws:iam::123456789012:role/elasticclaw-microvm"
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Ingress network connectors</label>
                  <textarea
                    value={formIngressConnectors}
                    onChange={e => setFormIngressConnectors(e.target.value)}
                    className="min-h-16 w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm font-mono"
                    placeholder="One ARN per line"
                  />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Egress network connectors</label>
                  <textarea
                    value={formEgressConnectors}
                    onChange={e => setFormEgressConnectors(e.target.value)}
                    className="min-h-16 w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm font-mono"
                    placeholder="One ARN per line"
                  />
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Idle max seconds</label>
                    <Input type="number" min={1} value={formIdleMaxDuration} onChange={e => setFormIdleMaxDuration(e.target.value)} className="h-8 text-sm" placeholder="900" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Suspended seconds</label>
                    <Input type="number" min={1} value={formSuspendedDuration} onChange={e => setFormSuspendedDuration(e.target.value)} className="h-8 text-sm" placeholder="300" />
                  </div>
                </div>
                <div className="grid grid-cols-3 gap-3">
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Max seconds</label>
                    <Input type="number" min={1} value={formMaximumDuration} onChange={e => setFormMaximumDuration(e.target.value)} className="h-8 text-sm" placeholder="28800" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Bridge port</label>
                    <Input type="number" min={1} value={formBridgePort} onChange={e => setFormBridgePort(e.target.value)} className="h-8 text-sm" placeholder="8080" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Token minutes</label>
                    <Input type="number" min={1} value={formAuthTokenExpiration} onChange={e => setFormAuthTokenExpiration(e.target.value)} className="h-8 text-sm" placeholder="30" />
                  </div>
                </div>
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={formAutoResume}
                    onChange={e => setFormAutoResume(e.target.checked)}
                    className="size-4 rounded border-border"
                  />
                  Auto resume suspended MicroVMs
                </label>
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
                disabled={saving || (modalMode === "add" && formProvider === "replicated" && !formToken) || (modalMode === "add" && formProvider === "daytona" && !formApiKey) || (modalMode === "add" && formProvider === "exedev" && (!formDefaultCpu || !formDefaultMemory || !formDefaultDisk)) || (formProvider === "lambda-microvms" && !formImageIdentifier.trim())}
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
  { value: "grok",       label: "Grok Build", placeholder: "xai-..." },
  { value: "ollama",     label: "Ollama",     placeholder: "ollama-local" },
  { value: "other",      label: "Other",      placeholder: "" },
]

const PROVIDER_MODELS: Record<string, LLMModelOption[]> = {
  anthropic: [
    { id: "anthropic/claude-opus-5",     name: "Claude Opus 5" },
    { id: "anthropic/claude-sonnet-5",   name: "Claude Sonnet 5" },
    { id: "anthropic/claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    { id: "anthropic/claude-opus-4-5",   name: "Claude Opus 4.5" },
    { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
    { id: "__custom",                    name: "Custom Anthropic model" },
  ],
  fireworks: [
    { id: "fireworks/accounts/fireworks/models/kimi-k2p7",                  name: "Kimi K2.7" },
    { id: "fireworks/accounts/fireworks/models/glm-5p2",                    name: "GLM 5.2" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-pro",            name: "DeepSeek V4 Pro" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-flash",          name: "DeepSeek V4 Flash" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p7",               name: "MiniMax M2.7" },
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
    { id: "openai/gpt-5.6-sol",   name: "GPT-5.6 Sol" },
    { id: "openai/gpt-5.6-terra", name: "GPT-5.6 Terra" },
    { id: "openai/gpt-5.6-luna",  name: "GPT-5.6 Luna" },
    { id: "openai/gpt-5.5",       name: "GPT-5.5" },
    { id: "__custom",             name: "Custom Codex model" },
  ],
  grok: [
    { id: "grok/grok-build-0.1", name: "Grok Build" },
    { id: "grok/grok-4.5",       name: "Grok 4.5" },
    { id: "grok/grok-4.3",       name: "Grok 4.3" },
    { id: "__custom",            name: "Custom Grok model" },
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
  const [formAuthProfile, setFormAuthProfile] = useState("")
  const [loginJob, setLoginJob] = useState<ModelAuthLoginJob | null>(null)
  const [loginError, setLoginError] = useState("")
  const [copiedLoginCode, setCopiedLoginCode] = useState(false)
  const loginWindowRef = useRef<Window | null>(null)

  const providerLabel = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.label ?? p
  const providerPlaceholder = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.placeholder ?? ""
  const providerModels = (p: string) => settings.modelOptions?.[p] ?? PROVIDER_MODELS[p] ?? []
  const supportsCLIAuth = formProvider === "codex" || formProvider === "grok"
  const authProfiles = (settings.modelAuthProfiles || []).filter(p => p.provider === formProvider)

  const resetForm = () => {
    setFormName(""); setFormProvider("anthropic"); setFormCustomProvider(""); setFormKey(""); setFormDefault(false); setFormDefaultModel(""); setFormCustomModel(""); setFormAuthProfile(""); setLoginJob(null); setLoginError(""); setCopiedLoginCode(false); setEditIdx(null)
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
    setFormAuthProfile(k.authProfile || "")
    const options = providerModels(k.provider)
    if (k.defaultModel && options.length > 0 && !options.some(m => m.id === k.defaultModel)) {
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
    const actualAuthProfile = supportsCLIAuth ? formAuthProfile.trim() : ""
    if (modalMode === "add") {
      if (!formName.trim() || (!formKey.trim() && actualProvider !== "ollama" && !actualAuthProfile)) return
      onSave({ llmKeys: [{ name: formName.trim(), provider: actualProvider, apiKey: formKey.trim(), default: formDefault, defaultModel: actualDefaultModel || undefined, authProfile: actualAuthProfile || undefined }] })
    } else if (editIdx !== null) {
      const existing = llmKeys[editIdx]
      const patch: Record<string, unknown> = {
        name: existing.name,          // lookup key — original name
        newName: formName.trim(),      // new name if changed
      }
      if (formKey.trim()) patch.apiKey = formKey.trim()
      patch.default = formDefault       // always send so user can unset
      if (actualDefaultModel) patch.defaultModel = actualDefaultModel
      patch.authProfile = actualAuthProfile
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

  function renderLoginWindow(win: Window, text: string) {
    win.document.body.style.margin = "0"
    win.document.body.style.background = "#0a0a0a"
    win.document.body.style.color = "#f5f5f5"
    win.document.body.style.fontFamily = "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace"
    win.document.body.style.whiteSpace = "pre-wrap"
    win.document.body.style.padding = "24px"
    win.document.body.textContent = text
  }

  function copyLoginCode(code: string) {
    navigator.clipboard.writeText(code).then(() => {
      setCopiedLoginCode(true)
      setTimeout(() => setCopiedLoginCode(false), 2000)
    })
  }

  async function doStartLogin() {
    const profile = formAuthProfile.trim() || `${formProvider}-default`
    setFormAuthProfile(profile)
    setLoginError("")
    loginWindowRef.current = window.open("", "_blank")
    if (loginWindowRef.current) {
      loginWindowRef.current.document.title = "Model login"
      renderLoginWindow(loginWindowRef.current, "Waiting for login URL...")
    }
    try {
      const job = await startModelAuthLogin(formProvider, profile)
      setLoginJob(job)
      if (job.url && loginWindowRef.current && !loginWindowRef.current.closed) {
        loginWindowRef.current.location.href = job.url
        loginWindowRef.current = null
      }
    } catch (err) {
      if (loginWindowRef.current && !loginWindowRef.current.closed) {
        loginWindowRef.current.close()
      }
      loginWindowRef.current = null
      setLoginError(err instanceof Error ? err.message : String(err))
    }
  }

  useEffect(() => {
    if (!loginJob || loginJob.status !== "running") return
    const timer = window.setInterval(async () => {
      try {
        const next = await fetchModelAuthLogin(loginJob.id)
        setLoginJob(next)
      } catch (err) {
        setLoginError(err instanceof Error ? err.message : String(err))
      }
    }, 1500)
    return () => window.clearInterval(timer)
  }, [loginJob])

  useEffect(() => {
    if (!loginJob?.url || !loginWindowRef.current || loginWindowRef.current.closed) return
    loginWindowRef.current.location.href = loginJob.url
    loginWindowRef.current = null
  }, [loginJob?.url])

  useEffect(() => {
    if (!loginJob || !loginWindowRef.current || loginWindowRef.current.closed || loginJob.url) return
    const lines = [`Login status: ${loginJob.status}`]
    if (loginJob.error) lines.push("", loginJob.error)
    if (loginJob.output) lines.push("", loginJob.output)
    renderLoginWindow(loginWindowRef.current, lines.join("\n"))
  }, [loginJob])

  const formProviderModels = providerModels(formProvider)

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
              {supportsCLIAuth && (
                <div className="space-y-2">
                  <label className="text-xs text-muted-foreground mb-1 block">CLI auth profile <span className="text-muted-foreground/60">(optional)</span></label>
                  <div className="flex gap-2">
                    <Input
                      value={formAuthProfile}
                      onChange={e => setFormAuthProfile(e.target.value)}
                      className="h-8 text-sm"
                      placeholder={`${formProvider}-default`}
                    />
                    <Button type="button" size="sm" variant="outline" onClick={doStartLogin} disabled={saving}>
                      Login
                    </Button>
                  </div>
                  {authProfiles.length > 0 && (
                    <select
                      value={formAuthProfile}
                      onChange={e => setFormAuthProfile(e.target.value)}
                      className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
                    >
                      <option value="">No CLI auth profile</option>
                      {authProfiles.map(p => (
                        <option key={p.name} value={p.name}>{p.name}{p.authenticated ? " (authenticated)" : ""}</option>
                      ))}
                    </select>
                  )}
                  {loginJob && (
                    <div className="rounded-md border border-border p-3 text-xs space-y-1">
                      <div className="text-muted-foreground">Login status: {loginJob.status}</div>
                      {loginJob.url && (
                        <a href={loginJob.url} target="_blank" rel="noopener noreferrer" className="underline break-all">{loginJob.url}</a>
                      )}
                      {loginJob.code && (
                        <div className="rounded-md border border-primary/30 bg-primary/10 p-2">
                          <div className="flex items-center justify-between gap-3">
                            <div className="min-w-0">
                              <div className="text-[11px] uppercase text-muted-foreground">One-time code</div>
                              <div className="font-mono text-lg font-semibold tracking-wide text-foreground">{loginJob.code}</div>
                            </div>
                            <Button type="button" size="sm" variant="outline" className="h-8 shrink-0 gap-1.5" onClick={() => copyLoginCode(loginJob.code || "")}>
                              {copiedLoginCode ? <Check className="size-3.5 text-green-500" /> : <Copy className="size-3.5" />}
                              {copiedLoginCode ? "Copied" : "Copy"}
                            </Button>
                          </div>
                        </div>
                      )}
                      {!loginJob.code && loginJob.output && (
                        <pre className="max-h-24 overflow-auto whitespace-pre-wrap break-words rounded border border-border bg-background/60 p-2 text-[11px] text-muted-foreground">{loginJob.output}</pre>
                      )}
                      {loginJob.error && <div className="text-red-400">{loginJob.error}</div>}
                      {loginJob.status === "complete" && <div className="text-green-400">Profile saved. Save this model key to use it.</div>}
                    </div>
                  )}
                  {loginError && <div className="text-xs text-red-400">{loginError}</div>}
                </div>
              )}
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default model <span className="text-muted-foreground/60">(optional)</span></label>
                {formProviderModels.length > 0 ? (
                  <select
                    value={formDefaultModel}
                    onChange={e => setFormDefaultModel(e.target.value)}
                    className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
                  >
                    <option value="">— use provider default —</option>
                    {formProviderModels.map(m => (
                      <option key={m.id} value={m.id}>{m.name}</option>
                    ))}
                  </select>
                ) : (
                  <Input value={formDefaultModel} onChange={e => setFormDefaultModel(e.target.value)} className="h-8 text-sm" placeholder="e.g. myprovider/model-name" />
                )}
                {formProviderModels.length > 0 && formDefaultModel === "__custom" && (
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
                  disabled={saving || !formName.trim() || (modalMode === "add" && !formKey.trim() && formProvider !== "ollama" && !formAuthProfile.trim()) || (formProvider === "other" && !formCustomProvider.trim())}
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
  const { appName: brandName } = useBranding()
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
  const [workspaceReloading, setWorkspaceReloading] = useState(true)
  // Which workspace the loaded apps belong to. While it differs from the current
  // workspace the list is (re)loading — derived instead of set inside the effect.
  const [loadedWorkspace, setLoadedWorkspace] = useState("")
  const workspaceLoading = workspaceReloading || (workspace !== "" && loadedWorkspace !== workspace)
  const [workspaceError, setWorkspaceError] = useState("")
  const visibleWorkspaceError = (workspace === "" || loadedWorkspace === workspace) ? workspaceError : ""
  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const resetModal = () => {
    setAppName(""); setAppId(""); setUrl(""); setInstallation(""); setPem("")
    setTestResult(null); setTestError(""); setTesting(false)
  }

  const openModal = () => { resetModal(); setShowModal(true) }
  const closeModal = () => { setShowModal(false); resetModal() }

  const workspaceGitHubPath = workspace ? `/api/workspaces/${encodeURIComponent(workspace)}/github-apps` : ""
  // Switching workspaces fires a new load without aborting the old one. Since
  // workspaceLoading is derived from loadedWorkspace, a late response for the
  // previous workspace would otherwise set loadedWorkspace backwards and pin
  // the panel in its loading state forever.
  const workspaceAppsRequest = useRef(0)
  const loadWorkspaceApps = useCallback(() => {
    if (!workspace) return Promise.resolve()
    const requestID = ++workspaceAppsRequest.current
    const stale = () => requestID !== workspaceAppsRequest.current
    return fetch(`${hubUrl}${workspaceGitHubPath}`, { headers: { Authorization: `Bearer ${token()}` } })
      .then(async (res) => {
        if (!res.ok) throw new Error(await res.text())
        return res.json()
      })
      .then((data) => {
        if (stale()) return
        setWorkspaceApps(data.githubApps || [])
        setWorkspaceError("")
      })
      .catch((e) => {
        if (stale()) return
        setWorkspaceError(e instanceof Error ? e.message : "Failed to load GitHub Apps")
      })
      .finally(() => {
        if (stale()) return
        setWorkspaceReloading(false)
        setLoadedWorkspace(workspace)
      })
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
      setWorkspaceReloading(true)
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
    setWorkspaceReloading(true)
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
          Register a GitHub App so your {brandName} workflows can access repositories.
          When an agent is created, it gets a scoped token that can read and write code,
          open pull requests, and check CI status — but only on repos the App is installed on.
        </p>
        <p>
          The App needs <strong>contents:write</strong>, <strong>pull_requests:write</strong>,
          and read access to <strong>metadata</strong>, <strong>checks</strong>, and <strong>statuses</strong>.
          Install it on your org or specific repos, then add the App ID and private key here.
        </p>
      </div>

      {visibleWorkspaceError && <p className="mb-4 text-sm text-destructive">{visibleWorkspaceError}</p>}

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
                  You haven&apos;t tested this GitHub App yet. We recommend clicking <strong>Test Permissions</strong> first to verify it works.
                </p>
              </>
            ) : (
              <>
                <div className="flex items-center gap-2">
                  <AlertTriangle className="size-5 text-yellow-400" />
                  <h3 className="font-medium">Permissions Missing</h3>
                </div>
                <p className="text-sm text-muted-foreground">
                  This GitHub App is missing required permissions. It <strong>will not work</strong> for agents until fixed.
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
                <p className="text-xs text-muted-foreground mt-1">Agent must have a tag like <code className="bg-muted px-1 rounded">user=alice</code> for that user to see it.</p>
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
type TrackerType = "linear" | "shortcut" | "github-issues" | "jira"

interface TrackerItem {
  type: TrackerType
  workspace: string
  tokenSet: boolean
  baseUrl?: string
  username?: string
}

function IntegrationsSection({ settings, onSave, saving, selectedWorkspace, hubPublicUrl }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean; selectedWorkspace: string; hubPublicUrl: string }) {
  const { appName } = useBranding()
  const [workspaceTrackers, setWorkspaceTrackers] = useState<TrackerItem[]>([])
  const [githubOwnerHint, setGithubOwnerHint] = useState("")
  const [reloading, setReloading] = useState(true)
  // Which workspace the loaded trackers belong to. While it differs from the
  // selected workspace the list is (re)loading — derived instead of set inside the effect.
  const [loadedWorkspace, setLoadedWorkspace] = useState("")
  const loading = reloading || (selectedWorkspace !== "" && loadedWorkspace !== selectedWorkspace)
  const [error, setError] = useState("")
  const visibleError = (selectedWorkspace === "" || loadedWorkspace === selectedWorkspace) ? error : ""
  const hubUrl = getHubUrl()
  const authToken = () => getAuthToken() || ""
  const issueTrackersPath = selectedWorkspace ? `/api/workspaces/${encodeURIComponent(selectedWorkspace)}/issue-trackers` : ""
  const linear = workspaceTrackers.filter(t => t.type === "linear")
  const shortcut = workspaceTrackers.filter(t => t.type === "shortcut")
  const githubIssues = workspaceTrackers.filter(t => t.type === "github-issues")
  const jira = workspaceTrackers.filter(t => t.type === "jira")

  const allTrackers: TrackerItem[] = workspaceTrackers

  // See the matching guard in GitHubSection: `loading` is derived from
  // loadedWorkspace, so a late response for a superseded workspace must not
  // write state or the panel stays stuck on the loading skeleton.
  const trackersRequest = useRef(0)
  const loadTrackers = useCallback(() => {
    if (!selectedWorkspace) return Promise.resolve()
    const requestID = ++trackersRequest.current
    const stale = () => requestID !== trackersRequest.current
    return fetch(`${hubUrl}${issueTrackersPath}`, { headers: { Authorization: `Bearer ${authToken()}` } })
      .then(async (res) => {
        if (!res.ok) throw new Error(await res.text())
        return res.json()
      })
      .then((data) => {
        if (stale()) return
        setWorkspaceTrackers(data.issueTrackers || [])
        setError("")
      })
      .catch((e) => {
        if (stale()) return
        setError(e instanceof Error ? e.message : "Failed to load issue trackers")
      })
      .finally(() => {
        if (stale()) return
        setReloading(false)
        setLoadedWorkspace(selectedWorkspace)
      })
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
  const [baseUrl, setBaseUrl] = useState("")
  const [username, setUsername] = useState("")
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
    setBaseUrl(""); setUsername(""); setToken(""); setWebhookSecret(""); setEditIdx(null); setEditType("linear"); setSetupTab("token")
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
    setBaseUrl(tracker.baseUrl || "")
    setUsername(tracker.username || "")
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
    if (type === "jira" && !baseUrl.trim()) {
      setError("Jira base URL is required")
      return
    }
    const trackerWorkspace = selectedWorkspace
    const originalWorkspace = modalMode === "edit" && editIdx !== null ? allTrackers[editIdx]?.workspace : ""
    setError("")
    const res = await fetch(`${hubUrl}${issueTrackersPath}`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${authToken()}`, "Content-Type": "application/json" },
      body: JSON.stringify({ type, workspace: trackerWorkspace, originalWorkspace, baseUrl: baseUrl.trim(), username: username.trim(), token: token.trim(), webhookSecret: webhookSecret.trim() }),
    })
    if (!res.ok) {
      setError(await res.text())
      return
    }
    setShowModal(false)
    resetModal()
    setReloading(true)
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
    setReloading(true)
    await loadTrackers()
  }

  const trackerTypeLabel = (t: TrackerType) => {
    switch (t) {
      case "linear": return "Linear"
      case "shortcut": return "Shortcut"
      case "github-issues": return "GitHub Issues"
      case "jira": return "Jira"
    }
  }

  const modalTitle = modalMode === "add"
    ? `Add ${trackerTypeLabel(modalType)}`
    : `Edit ${trackerTypeLabel(editType)}`

  const modalIcon = (modalMode === "add" ? modalType : editType) === "linear"
    ? <Zap className="size-4" />
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <span className="text-[#F4603C]">⚡</span>
    : (modalMode === "add" ? modalType : editType) === "jira"
    ? <span className="text-[#0052CC] font-semibold text-sm">J</span>
    : <Github className="size-4" />

  const githubIssuesTokenParams = new URLSearchParams({
    name: `${appName} GitHub Issues`,
    description: `Allows ${appName} to read and update GitHub issues`,
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
    ? <>Use a Shortcut API token from Shortcut settings. The token lets {appName} read and update stories.</>
    : (modalMode === "add" ? modalType : editType) === "github-issues"
    ? <>Use a <a href={githubIssuesTokenUrl} target="_blank" rel="noopener noreferrer" className="underline">fine-grained GitHub PAT</a> for issue API actions. {githubOwnerHint ? <>The link starts with <code>{githubOwnerHint}</code> as the resource owner. </> : null}Grant repository access to the repos this workspace watches.</>
    : (modalMode === "add" ? modalType : editType) === "jira"
    ? <>Use a Jira personal access token or API token with issue read/write permissions.</>
    : null
  const activeTrackerType = modalMode === "add" ? modalType : editType
  const canGenerateWebhookSecret = activeTrackerType === "github-issues" || activeTrackerType === "shortcut" || activeTrackerType === "jira"
  const workspaceWebhookBase = hubPublicUrl || hubUrl
  const linearWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/linear`
  const githubIssuesWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/github-issues`
  const jiraWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/jira`

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
          {jira.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Jira: {jira.length}</span>
          )}
        </div>
      </div>

      {visibleError && <p className="text-sm text-destructive">{visibleError}</p>}

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
                ) : tracker.type === "jira" ? (
                  <span className="text-[#0052CC] font-semibold text-sm">J</span>
                ) : (
                  <Github className="size-4 text-muted-foreground" />
                )}
                <div>
                  <p className="text-sm font-medium">{tracker.workspace}</p>
                  <div className="flex items-center gap-1.5 mt-0.5">
                    <span className="text-xs text-muted-foreground capitalize">{tracker.type === "github-issues" ? "github issues" : tracker.type}</span>
                    {tracker.type === "jira" && tracker.baseUrl ? (
                      <>
                        <span className="text-xs text-muted-foreground">·</span>
                        <span className="text-xs text-muted-foreground">{tracker.baseUrl}</span>
                      </>
                    ) : null}
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
            <button
              onClick={() => openAdd("jira")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <span className="text-[#0052CC] font-semibold text-sm">J</span>
              <span>Jira</span>
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
                        : `Connect Linear for this workspace. The API key lets ${appName} read issues and move them between statuses.`}
                    </p>
                    {setupTab === "token" ? (
                    <div className="mt-4 space-y-4">
                      <div>
                        <h4 className="text-sm font-medium">{activeTrackerType === "github-issues" ? "GitHub PAT" : "Linear API Token"}</h4>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? `Used by ${appName} to read and update issues.`
                            : `Used by ${appName} to read Linear issues and move them between statuses.`}
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
                  <p>Connect Shortcut for this workspace. The API token lets {appName} read stories and update workflow states.</p>
                  <p>Create a Shortcut webhook using the Shortcut URL from the Webhooks page. If Shortcut signs the payload with a secret, paste that same secret below.</p>
                </div>
              ) : activeTrackerType === "jira" ? (
                <div className="space-y-2 text-sm text-muted-foreground">
                  <p>Connect Jira for this workspace. The token lets {appName} read issues, add comments, and transition statuses.</p>
                  <p>Create a Jira Automation rule that sends a web request with the Issue data automation payload, then use the payload URL below.</p>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">
                  Connect {trackerTypeLabel(activeTrackerType)} for this workspace.
                </p>
              )}
              {activeTrackerType !== "github-issues" && activeTrackerType !== "linear" && (
                <>
              {activeTrackerType === "jira" && (
                <>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Jira Base URL</label>
                    <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)} className="h-9 text-sm" placeholder="https://jira.example.com" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Username <span className="text-muted-foreground/60">(optional)</span></label>
                    <Input value={username} onChange={e => setUsername(e.target.value)} className="h-9 text-sm" placeholder="admin@example.com" />
                    <p className="text-xs text-muted-foreground mt-1">Set for basic auth. Leave blank to send the token as a bearer token.</p>
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Payload URL</label>
                    <div className="flex gap-2">
                      <Input readOnly value={jiraWebhookUrl} className="h-9 text-xs font-mono" />
                      <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(jiraWebhookUrl, "jira-url")}>
                        {copiedSetup === "jira-url" ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                      </Button>
                    </div>
                    <p className="text-xs text-muted-foreground mt-1">Use this URL from Jira Automation&apos;s Send web request action with the Issue data automation payload.</p>
                  </div>
                </>
              )}
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
                    : activeTrackerType === "jira"
                    ? "Generate one here, then send it in the X-ElasticClaw-Webhook-Secret header."
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
                <Button size="sm" disabled={saving || (modalMode === "add" && !token.trim()) || (activeTrackerType === "jira" && !baseUrl.trim())} onClick={saveTracker}>
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
      name: "Jira",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/jira` : "",
      hint: "Use in Jira webhook settings. Subscribe to issue created and issue updated events.",
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

  // Only called from the mount effect, where loading/error already hold their
  // initial values — so no state needs to be set before the fetch resolves.
  const load = useCallback(
    () => fetchWorkspaces()
      .then((data) => setWorkspaces(data))
      .catch((e) => setError(e instanceof Error ? e.message : "Failed to load workspaces"))
      .finally(() => setLoading(false)),
    [],
  )

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

  // Only called from the mount effect, where loading/error already hold their
  // initial values — so no state needs to be set before the fetch resolves.
  const load = useCallback(
    () => fetchWorkspaces()
      .then((data) => setWorkspaces(data))
      .catch((e) => setError(e instanceof Error ? e.message : "Failed to load workflows"))
      .finally(() => setLoading(false)),
    [],
  )

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
      <div className="flex flex-col flex-wrap gap-3 md:flex-row md:items-start md:justify-between">
        <div className="flex-auto flex-shrink-0">
          <p className="text-sm font-medium">
            <WorkflowName name={workflow.name} />
          </p>
          <p className="text-xs text-muted-foreground truncate">
            {workflow.workspaceName} · {workflow.integration || "manual"}
            {workflow.triggerStatus ? ` · ${workflow.triggerStatus}` : ""}
            {workflow.projects?.length ? ` · projects: ${workflow.projects.join(", ")}` : ""}
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
          <WorkflowRunsDialog workflow={workflow} />
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
  const [reloading, setReloading] = useState(true)
  const [loadedPath, setLoadedPath] = useState("")

  // Two stores live behind this screen and only one of them was reachable: the
  // workspace file, and hub.yaml's top-level `secrets`. Hub secrets are what
  // notifier token_secret references resolve against, so without this scope a
  // hub with any workspace has no way at all to create the secret the Notifier
  // screen requires.
  //
  // The default tracks the operator's CHOICE, not the `workspace` prop: the
  // workspace list is fetched after the first render, so seeding state from the
  // prop would latch "Hub" on every direct load of /settings/secrets and write
  // hub.yaml secrets while the screen still said Workspace was available.
  const [scopeChoice, setScopeChoice] = useState<"workspace" | "hub" | null>(null)
  const scoped = Boolean(workspace) && scopeChoice !== "hub"

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""
  const secretsPath = scoped ? `/api/workspaces/${encodeURIComponent(workspace)}/secrets` : "/api/secrets"
  // Switching scope must not leave the other store's names on screen while the
  // new one loads. Derived from the path the list was last loaded for rather
  // than set at the head of `refresh`, which the mount effect calls straight
  // from its body — a synchronous setState there is what
  // react-hooks/set-state-in-effect rejects.
  const loading = reloading || loadedPath !== secretsPath

  // The scope switch changes `secretsPath` within one mount, so two refreshes
  // can be in flight at once and resolve out of order. The loser would render
  // one store's names under the other store's heading — and Delete, which uses
  // the render-time path, would then aim at the wrong endpoint.
  const refreshGeneration = useRef(0)

  const refresh = useCallback(async () => {
    const generation = ++refreshGeneration.current
    try {
      const res = await fetch(`${hubUrl}${secretsPath}`, { headers: { Authorization: `Bearer ${token()}` } })
      if (res.ok) {
        const data = await res.json()
        if (generation !== refreshGeneration.current) return
        setSecrets(data.secrets || [])
        setError(null)
      } else {
        const message = await res.text()
        if (generation !== refreshGeneration.current) return
        // Keeping the other store's names on screen is worse than an empty
        // list: they read as this scope's secrets, and a workspace name picked
        // as a notifier token_secret is one the hub can never resolve.
        setError(message)
        setSecrets([])
      }
    } finally {
      if (generation === refreshGeneration.current) {
        setLoadedPath(secretsPath)
        setReloading(false)
      }
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
        <p className="text-sm text-muted-foreground mb-4">
          {scoped
            ? <>Named secrets for workspace <code className="bg-muted px-1 rounded text-xs">{workspace}</code>. Values are stored on the hub and referenced from workspace env or workflow secret refs.</>
            : <>Hub-wide secrets, stored in the hub config. These are what hub-level references resolve against — notifier bot tokens (Settings → Notifier) among them.</>}
        </p>
        {Boolean(workspace) && (
          <div className="flex items-center gap-1 mb-6">
            <Button size="sm" variant={scoped ? "secondary" : "ghost"} onClick={() => setScopeChoice("workspace")}>Workspace</Button>
            <Button size="sm" variant={scoped ? "ghost" : "secondary"} onClick={() => setScopeChoice("hub")}>Hub</Button>
          </div>
        )}
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
  // Restore from sessionStorage after mount (client-only).
  // The values must be READ synchronously here: the persist effects below run in
  // the same commit and overwrite storage with the initial (empty) state.
  // Applying them to React state is deferred to a microtask so no setState runs
  // synchronously inside the effect body. Intentionally not cancelled on cleanup:
  // under StrictMode's double effect run, only the first run can still see the
  // stored values, so its deferred restore must be allowed to apply.
  useEffect(() => {
    let raw: string | null = null
    let yaml: string | null = null
    let backup: string | null = null
    try {
      raw = sessionStorage.getItem(SS_CHAT_KEY)
      yaml = sessionStorage.getItem(SS_YAML_KEY)
      backup = sessionStorage.getItem(SS_BACKUP_KEY)
    } catch { /* ignore */ }
    Promise.resolve().then(() => {
      try {
        if (raw) setMessages((JSON.parse(raw) as ChatMessage[]).map(normalizeStoredMessage))
        if (yaml) {
          setProposedYaml(yaml)
          setPlaceholders(extractYamlPlaceholders(yaml))
          setSecretValues({})
        }
        if (backup) setBackupPath(backup)
      } catch { /* ignore */ }
    })
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
          Model Context Protocol servers add tools to your agents. Configure them here and reference them in workflows.
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

// ── Notifier ─────────────────────────────────────────────────────────────────

// Labels mirror the headlines the hub actually posts, so what an operator
// checks here is what they will read in the channel. The list is deliberately
// the hub's routable vocabulary (types.LifecycleEventTypes) and nothing more:
// the concrete failure kinds ("Couldn't get a machine", "Agent ran out of
// time", ...) are how ONE agent_stopped event is titled, never event types of
// their own, so offering them as separate checkboxes built routes that could
// never fire.
const LIFECYCLE_EVENT_LABELS: Record<string, string> = {
  agent_started: "Agent started",
  pr_opened: "PR opened",
  agent_idle: "Agent stalled",
  stage_stalled: "Pipeline stage stalled",
  agent_stopped: "Agent died or failed",
  done_without_pr: "Agent finished without a PR",
}

type LifecycleCategory = keyof LifecycleEventToggles

// Which global toggle mutes each event type. Anything the hub adds later that
// is not listed here is treated as always-on rather than silently muted.
const LIFECYCLE_EVENT_CATEGORY: Record<string, LifecycleCategory> = {
  agent_started: "agentStarted",
  pr_opened: "prOpened",
  agent_idle: "agentIdle",
  stage_stalled: "stageStalled",
  agent_stopped: "failures",
  done_without_pr: "failures",
}

const LIFECYCLE_CATEGORIES: { id: LifecycleCategory; label: string; description: string }[] = [
  { id: "agentStarted", label: "Agent started", description: "An agent picked up a ticket and started working" },
  { id: "prOpened", label: "PR opened", description: "An agent opened a pull request" },
  { id: "failures", label: "Failures", description: "Crashes, timeouts, lost machines, finished without a PR" },
  { id: "agentIdle", label: "Agent stalled", description: "An agent stopped making progress" },
  { id: "stageStalled", label: "Pipeline stage stalled", description: "A pipeline stage stopped making meaningful progress" },
]

// Fallback only — the canonical list comes from settings.lifecycleEventTypes.
const FALLBACK_LIFECYCLE_EVENT_TYPES = Object.keys(LIFECYCLE_EVENT_CATEGORY)

// Slack conversation IDs: public channels (C…), private groups (G…), DMs (D…).
const SLACK_CHANNEL_ID_RE = /^[CGD][A-Za-z0-9]+$/

function lifecycleEventLabel(eventType: string): string {
  return LIFECYCLE_EVENT_LABELS[eventType] || eventType.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase())
}

async function sendTestNotification(eventType: string, via: string): Promise<void> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/notifications/test`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    // via picks which configured notifier to probe; the hub falls back to the
    // first effective route when it is omitted.
    body: JSON.stringify({ event_type: eventType, dry_run: false, via }),
  })
  const raw = await res.text()
  if (res.ok) return
  let message = raw
  try {
    const parsed = JSON.parse(raw) as { error?: string }
    if (parsed.error) message = parsed.error
  } catch {
    // Not JSON — surface the raw body.
  }
  throw new Error(message || `Test send failed (${res.status})`)
}

// The reports this hub can schedule. Kept as a const list rather than read
// from the config: a schedule naming a report the hub does not carry is
// rejected on save, so offering only what exists is what keeps the editor from
// producing one.
const SCHEDULED_REPORTS: { id: string; label: string; description: string }[] = [
  {
    id: "pending_prs",
    label: "Pull requests waiting for review",
    description: "Every open pull request an agent has left waiting, grouped by ticket.",
  },
]

function scheduledReportLabel(report: string): string {
  return SCHEDULED_REPORTS.find((r) => r.id === report)?.label || report
}

// An empty weekday list is the hub's "every day", never "never".
function weekdaysLabel(weekdays: string[]): string {
  if (weekdays.length === 0) return "Every day"
  return WEEKDAYS.filter((day) => weekdays.includes(day.id)).map((day) => day.label).join(", ")
}

// Wire values are the three-letter names the hub validates against; the labels
// are what the chips show.
const WEEKDAYS: { id: string; label: string }[] = [
  { id: "mon", label: "Mon" },
  { id: "tue", label: "Tue" },
  { id: "wed", label: "Wed" },
  { id: "thu", label: "Thu" },
  { id: "fri", label: "Fri" },
  { id: "sat", label: "Sat" },
  { id: "sun", label: "Sun" },
]

// Every IANA zone the browser knows. Intl.supportedValuesOf is recent enough
// that a hub opened in an older browser must still get a usable list, so the
// fallback carries the viewer's own zone and UTC — the two a schedule is
// realistically written in.
function timezoneOptions(): string[] {
  try {
    const supported = (Intl as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf
    if (supported) return supported.call(Intl, "timeZone")
  } catch {
    // Fall through to the minimal list.
  }
  return [...new Set([browserTimezone(), "UTC"])]
}

function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
  } catch {
    return "UTC"
  }
}

// The result of probing one scheduled report. `payload` is set only by a dry
// run — the rendered message the hub would post, shown instead of sending it.
type ReportTestState = {
  status: "sending" | "ok" | "error"
  message: string
  payload?: unknown
}

async function sendScheduledReportTest(
  report: string,
  via: string,
  dryRun: boolean,
): Promise<{ empty: boolean; payload?: unknown }> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/notifications/test`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify({ report, via, dry_run: dryRun }),
  })
  const raw = await res.text()
  let parsed: { error?: string; empty?: boolean; payload?: unknown } = {}
  try {
    parsed = JSON.parse(raw)
  } catch {
    // Not JSON — the raw body is the only thing to report.
  }
  if (!res.ok) throw new Error(parsed.error || raw || `Test send failed (${res.status})`)
  return { empty: Boolean(parsed.empty), payload: parsed.payload }
}

// Turns a Slack payload into the lines an operator reads, so a preview shows
// the message rather than its wire encoding. Anything this cannot walk falls
// back to the raw JSON, which is still the honest answer.
function previewLines(payload: unknown): string[] {
  const attachments = (payload as { attachments?: { blocks?: { text?: { text?: string } }[] }[] })?.attachments
  const lines: string[] = []
  for (const attachment of attachments || []) {
    for (const block of attachment.blocks || []) {
      const text = block.text?.text
      if (text) lines.push(text)
    }
  }
  if (lines.length > 0) return lines
  return [JSON.stringify(payload, null, 2)]
}

type TestState = { status: "sending" | "ok" | "error"; message: string }

// Records that the Notifier screen itself cleared `enabled` because the route
// set went empty — the pause is ours, not the operator's, and the next save
// that routes a channel must lift it. It lives in localStorage rather than in
// component state because NotifierSection unmounts on every navigation inside
// Settings (and on reload): a clamp forgotten there latches the `enabled:false`
// this screen wrote on its own initiative, so re-routing a channel would no
// longer restore alerts — exactly what the edit dialog promises it does.
// localStorage rather than sessionStorage because the clamp outlives the
// browsing context that wrote it: the repair is routinely finished in another
// tab, or after a browser restart, and a per-tab clamp would be gone by then.
// The stored value is the hub URL, so the clamp never leaks across hubs.
const CLAMPED_PAUSE_STORAGE_KEY = "elasticclaw:notifier-clamped-pause"

function readClampedPause(): boolean {
  try {
    return localStorage.getItem(CLAMPED_PAUSE_STORAGE_KEY) === getHubUrl()
  } catch {
    return false
  }
}

function writeClampedPause(clamped: boolean): void {
  try {
    if (clamped) localStorage.setItem(CLAMPED_PAUSE_STORAGE_KEY, getHubUrl())
    else localStorage.removeItem(CLAMPED_PAUSE_STORAGE_KEY)
  } catch {
    // Storage unavailable (private mode): the clamp degrades to not existing,
    // which is what the screen did before it was persisted at all.
  }
}

function NotifierSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => Promise<SaveOutcome>; saving: boolean }) {
  const lifecycle = settings.notifications?.lifecycle
  const notifiers = settings.notifications?.notifiers || {}
  const schedules = settings.notifications?.scheduled || []
  const names = Object.keys(notifiers).sort((a, b) => a.localeCompare(b))
  const eventTypes = settings.lifecycleEventTypes?.length ? settings.lifecycleEventTypes : FALLBACK_LIFECYCLE_EVENT_TYPES

  const enabled = lifecycle?.enabled ?? false
  const categoryEnabled: Record<LifecycleCategory, boolean> = {
    agentStarted: lifecycle?.events?.agentStarted ?? true,
    prOpened: lifecycle?.events?.prOpened ?? true,
    failures: lifecycle?.events?.failures ?? true,
    agentIdle: lifecycle?.events?.agentIdle ?? true,
    stageStalled: lifecycle?.events?.stageStalled ?? true,
  }
  // A legacy single-channel `via` reads as one route over every event; saving
  // any change migrates it to routes. `via` is trimmed exactly where the hub
  // trims it (ValidateNotificationsConfig, lifecycleNotifierTick): a `via` with
  // surrounding whitespace is valid on disk and delivers normally, so matching
  // it raw here would render a working channel as an unroutable orphan whose
  // only offered remedy — "Remove route" — silently pauses alerts hub-wide.
  const routes: LifecycleRouteView[] = (
    lifecycle?.routes?.length
      ? lifecycle.routes
      : lifecycle?.via
        ? [{ via: lifecycle.via, events: [] }]
        : []
  ).map((route) => ({ ...route, via: (route.via || "").trim() }))
  const routeFor = (name: string) => routes.find((r) => r.via === name)
  // The types a route names that this hub actually supports, and the rest.
  // ValidateNotificationsConfig checks routes[].events against
  // IsLifecycleEventType only AFTER the disabled-lifecycle short-circuit, so a
  // hand-written hub.yaml with alerts paused can legitimately hold names the
  // hub no longer knows. The dialog can only ever render supported types, so
  // counting the others would make the card badge disagree with the checkboxes
  // and let a test send pick an event the hub rejects.
  const knownEvents = (events?: string[]) => (events || []).filter((eventType) => eventTypes.includes(eventType))
  const unknownEvents = (events?: string[]) => (events || []).filter((eventType) => !eventTypes.includes(eventType))
  // The predicate saveChannel stores by: an allow-list naming every type is no
  // filter at all. Shared by the card badge and the dialog so the same route
  // cannot read as filtered on one and unfiltered on the other. Membership, not
  // length: a hand-written list of five names the hub no longer knows is not
  // "all alerts", and reading it as one would rewrite the route to receive-all
  // on the next save instead of dropping just the stale entries.
  const isAllAlerts = (events?: string[]) => !events?.length || eventTypes.every((eventType) => events.includes(eventType))
  // A route whose notifier is gone. The hub deliberately accepts this on disk
  // while alerts are paused ("an operator who mutes alerts and then deletes the
  // notifier must not be left with a hub that refuses to load"), but it rejects
  // the same block the moment alerts are enabled — so the screen has to render
  // the route and offer a way to drop it. Silently dropping it in buildPatch
  // would delete config the operator never saw.
  // One entry per via: "Remove route" drops every route naming it, and a
  // hand-written block can name the same missing notifier twice.
  const orphanRoutes = routes.filter(
    (route, i) => !notifiers[route.via] && routes.findIndex((other) => other.via === route.via) === i,
  )
  // A route whose allow-list names alert types this hub does not support. The
  // hub accepts it on disk while alerts are paused — routes are only validated
  // once enabled — and rejects the whole block the moment they are turned on,
  // exactly like an orphan route, so the master switch has to be gated on it
  // too rather than offering a save that is certain to fail.
  const unsupportedRoutes = routes.filter((route) => unknownEvents(route.events).length > 0)
  // A `via` routed twice, or an allow-list naming the same alert type twice.
  // Both are rejected only once alerts are enabled, so a hand-written block can
  // hold them while paused — and the screen renders one card per notifier
  // showing only the FIRST matching route, so the hub's error names a routes[]
  // index that is nowhere on the page. Gated like the orphan and unsupported
  // cases; saveChannel collapses both so "open Edit and save" actually clears it.
  const duplicatedVias = new Set(routes.map((route) => route.via).filter((via, i, all) => all.indexOf(via) !== i))
  const hasDuplicateEvents = (events?: string[]) => new Set(events || []).size !== (events || []).length
  const isDuplicated = (name: string) => duplicatedVias.has(name) || hasDuplicateEvents(routeFor(name)?.events)
  const duplicateRoutes = routes.filter((route) => duplicatedVias.has(route.via) || hasDuplicateEvents(route.events))
  // A hub that has never had a lifecycle block: routing its first channel turns
  // alerts on. This is deliberately NOT inferred from (enabled=false, routes=[]),
  // which an operator reaches by muting the master switch and then removing the
  // last channel — see clampedPause.
  const neverConfigured = !lifecycle
  const secretNames = [...(settings.secrets || [])].sort((a, b) => a.localeCompare(b))
  // A token_secret naming a hub secret that no longer exists. The <select> below
  // would otherwise render blank while still holding the dangling name, so Save
  // writes the broken reference back with nothing on screen saying why the
  // channel never delivers.
  const secretMissing = (name?: string) => Boolean(name) && !secretNames.includes(name as string)

  const isEventMuted = (eventType: string) => {
    const category = LIFECYCLE_EVENT_CATEGORY[eventType]
    return category ? !categoryEnabled[category] : false
  }

  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [editName, setEditName] = useState<string | null>(null)
  const [formName, setFormName] = useState("")
  const [formChannel, setFormChannel] = useState("")
  const [formTokenSecret, setFormTokenSecret] = useState("")
  // The hub refuses to build a slack notifier whose min_send_interval it cannot
  // parse, and re-checks it on every save that touches the channel. A hub.yaml
  // written by hand can hold such a value, so the screen must offer a way to
  // repair it: without this field the save can never be made to pass, and a
  // notifier a pipeline notifies through cannot be removed either.
  const [formMinSendInterval, setFormMinSendInterval] = useState("")
  const [formRouted, setFormRouted] = useState(true)
  const [formEvents, setFormEvents] = useState<string[]>([])
  // Allow-list entries the edited route holds that this hub does not support.
  // They are kept out of formEvents — no checkbox can ever represent them — so
  // the dialog has to name them itself, or saving drops config the operator
  // never saw and the master switch later 400s on a value the screen never
  // showed.
  const [formDroppedEvents, setFormDroppedEvents] = useState<string[]>([])
  const [formError, setFormError] = useState("")
  const [tests, setTests] = useState<Record<string, TestState>>({})
  // Save failures the dialog can no longer show, keyed by the channel the save
  // was for. A rejected PATCH whose dialog has meanwhile been replaced has
  // nowhere else to land: the page-level banner lives under the Radix overlay,
  // so dropping the message leaves the operator believing a save that failed
  // went through.
  const [saveErrors, setSaveErrors] = useState<Record<string, string>>({})
  // The same failure for a channel that has no card to land on: a rejected Add
  // created no notifier, so keying it into saveErrors above would render it
  // nowhere at all. Painted both inside the dialog and above the channel list,
  // since the operator may close the dialog that replaced the failed one.
  const [detachedSaveError, setDetachedSaveError] = useState("")

  // Reconcile the stored clamp against the config actually loaded: it records a
  // pause this screen imposed for an empty route set, so a config that HAS
  // routes has already been repaired — by this browser or any other — and the
  // flag must go before it can lift an `enabled:false` the operator chose.
  useEffect(() => {
    if (routes.length > 0) writeClampedPause(false)
  }, [routes.length])

  const resetForm = () => {
    setFormName(""); setFormChannel(""); setFormTokenSecret(""); setFormMinSendInterval("")
    setFormRouted(true); setFormEvents([]); setFormDroppedEvents([]); setFormError(""); setEditName(null)
  }

  // Identifies the dialog a save was started from. An in-flight PATCH must not
  // close, reset, or drop its error into a dialog the operator has meanwhile
  // reopened on another channel — that discards unsaved form state and blames
  // the wrong channel.
  const saveGeneration = useRef(0)

  // Invalidates in-flight test sends. A result that lands after the channel's
  // destination or routing changed describes a message that went somewhere else
  // — exactly the stale state every setTests() below clears.
  //
  // Saving ONE channel leaves every other channel's notifier and route
  // byte-identical, so their in-flight results are still accurate: invalidating
  // them would throw away a real failure ("channel_not_found") with nothing on
  // the card to replace it. Per-channel bumps cover a single channel's save or
  // removal; the hub-wide counter covers the master switch and the category
  // toggles, which change what every channel receives.
  const testGeneration = useRef<Record<string, number>>({})
  const testGenerationAll = useRef(0)
  const testStamp = (name: string) => `${testGenerationAll.current}:${testGeneration.current[name] ?? 0}`
  const invalidateTest = (name: string) => {
    testGeneration.current[name] = (testGeneration.current[name] ?? 0) + 1
  }

  const openAdd = () => { saveGeneration.current++; resetForm(); setModalMode("add"); setShowModal(true) }
  const openEdit = (name: string) => {
    saveGeneration.current++
    const notifier = notifiers[name]
    const route = routeFor(name)
    setFormName(name)
    setFormChannel(notifier.channel || "")
    setFormTokenSecret(notifier.token_secret || "")
    setFormMinSendInterval(notifier.min_send_interval || "")
    setFormRouted(Boolean(route))
    // Only supported types can be checked, so only those are seeded; the rest
    // are surfaced separately rather than carried invisibly back out on save.
    // De-duplicated because a checkbox can only be on or off: a type listed
    // twice on disk would otherwise be counted twice in the dialog summary and
    // re-sent verbatim by a save that changed nothing.
    setFormEvents([...new Set(knownEvents(route?.events))])
    setFormDroppedEvents(unknownEvents(route?.events))
    setFormError("")
    setEditName(name)
    setModalMode("edit")
    setShowModal(true)
  }

  // PATCH /api/settings replaces the whole notifications block, so every save
  // rebuilds it from the current view plus the change being made. The clamp the
  // patch implies is returned, never written here: see savePatch.
  function buildPatch(next: {
    notifiers?: Record<string, NotifierView>
    enabled?: boolean
    routes?: LifecycleRouteView[]
    events?: Record<LifecycleCategory, boolean>
    scheduled?: ScheduledNotificationView[]
  }): { patch: object; clamp: boolean } {
    const outNotifiers: Record<string, Record<string, string>> = {}
    for (const [name, notifier] of Object.entries(next.notifiers ?? notifiers)) {
      const out: Record<string, string> = { type: notifier.type || "slack" }
      if (notifier.channel) out.channel = notifier.channel
      if (notifier.token_secret) out.token_secret = notifier.token_secret
      if (notifier.api_base) out.api_base = notifier.api_base
      // An emptied interval is sent as "" rather than omitted when the stored
      // notifier has one: the hub folds a patch over the settings already on
      // disk, so omitting the key would keep the value the operator just
      // cleared — and keep rejecting the save if that value is unparseable.
      const interval = notifier.min_send_interval ?? ""
      if (interval || notifiers[name]?.min_send_interval) out.min_send_interval = interval
      outNotifiers[name] = out
    }
    // Always an array: sending routes is what clears the legacy `via`.
    const outRoutes = (next.routes ?? routes).map((route) => ({
      via: route.via,
      events: route.events?.length ? route.events : [],
    }))
    // The hub rejects an enabled lifecycle block with no routes ("via is
    // required when enabled") and this screen never exposes `via`, so losing the
    // last route pauses alerts instead of failing the save with a message about
    // a field that is not on the page. That pause is ours, not the operator's,
    // and the clamp lifts it on the next save that routes a channel again.
    // Without it the `false` we wrote latches — every later save re-sends the
    // stale value read back from GET — and a plain channel swap silently mutes
    // the hub forever, contradicting the dialog's "until another channel is
    // routed". The flag is recorded here rather than inferred from the reloaded
    // config so a deliberate master-switch OFF survives a channel swap.
    // The clamp only carries meaning while the LOADED config still has no
    // routes: once routing is restored anywhere else (hub.yaml, the CLI, a
    // second operator, another device) the hub holds a pause the operator owns,
    // and a stale flag here would flip alerts back on from the next unrelated
    // save with nothing on screen announcing it.
    const clampActive = routes.length === 0 && readClampedPause()
    const wantEnabled = next.enabled ?? (enabled || clampActive || neverConfigured)
    const outLifecycle: Record<string, unknown> = {
      enabled: outRoutes.length > 0 && wantEnabled,
      routes: outRoutes,
      events: next.events ?? categoryEnabled,
    }
    if (lifecycle?.pollInterval) outLifecycle.pollInterval = lifecycle.pollInterval
    if (lifecycle?.idleAfter) outLifecycle.idleAfter = lifecycle.idleAfter
    if (lifecycle?.stageProgressAfter) outLifecycle.stageProgressAfter = lifecycle.stageProgressAfter
    // Always sent, even by a save that only touches a channel: PATCH replaces
    // the whole notifications block, so omitting it would delete every
    // scheduled report the first time anyone edits a Slack channel.
    const outScheduled = (next.scheduled ?? schedules).map((schedule) => ({
      id: schedule.id,
      report: schedule.report,
      via: schedule.via,
      at: schedule.at,
      timezone: schedule.timezone || "",
      weekdays: schedule.weekdays,
      enabled: schedule.enabled,
    }))
    return {
      patch: { notifications: { notifiers: outNotifiers, lifecycle: outLifecycle, scheduled: outScheduled } },
      // The never-configured hub is clamped too, and for the same reason: the
      // block this save creates carries `enabled:false` only because it has no
      // route to send to, which is this screen's own doing — the operator was
      // adding a channel, not muting a hub that had nothing to mute. Without
      // the flag, routing that channel later finds `enabled:false` in the
      // loaded config, keeps it, and leaves the freshly routed channel paused
      // until the master switch is found by hand.
      clamp: outRoutes.length === 0 && wantEnabled,
    }
  }

  // The clamp describes what the hub holds, so it is written only once the
  // PATCH has actually landed. Recording it while building the body loses the
  // flag on a save that fails — the hub keeps the `enabled:false` this screen
  // wrote earlier, the retry no longer reads a clamp to lift it, and every
  // later save re-sends that stale `false`, muting the hub for good.
  // The flag describes what the HUB holds, so it follows the PATCH, not the
  // re-read that comes after it: a save whose follow-up GET failed still left
  // the hub with the `enabled:false` this screen wrote on its own initiative,
  // and losing the clamp there latches that `false` for good.
  async function savePatch(next: Parameters<typeof buildPatch>[0]): Promise<SaveOutcome> {
    const { patch, clamp } = buildPatch(next)
    const outcome = await onSave(patch)
    if (outcome.persisted) writeClampedPause(clamp)
    return outcome
  }

  async function saveChannel() {
    const name = formName.trim()
    const channel = formChannel.trim()
    if (!name) { setFormError("Name is required."); return }
    if (modalMode === "add" && notifiers[name]) { setFormError(`A channel named "${name}" already exists.`); return }
    if (!channel) { setFormError("Channel ID is required."); return }
    if (channel.startsWith("#") || !SLACK_CHANNEL_ID_RE.test(channel)) {
      setFormError(
        `"${channel}" is not a Slack channel ID. Use the ID (e.g. C0123ABCD), not the #name — you'll find it at the bottom of the channel's details dialog in Slack.`,
      )
      return
    }
    if (!formTokenSecret) { setFormError("Pick the hub secret holding the Slack bot token."); return }
    setFormError("")

    const existing = editName ? notifiers[editName] : undefined
    const nextNotifiers: Record<string, NotifierView> = {
      ...notifiers,
      [name]: {
        ...existing,
        type: existing?.type || "slack",
        channel,
        token_secret: formTokenSecret,
        min_send_interval: formMinSendInterval.trim(),
      },
    }
    // Every type checked is the same thing as no filter; store it as one so a
    // later-added alert type keeps reaching the channel. formEvents holds only
    // supported types, so any unsupported entry the route arrived with is
    // dropped here — the dialog says so before the operator saves.
    // De-duplicated for the same reason the dialog seeds a Set: the hub rejects
    // a repeated event once alerts are enabled, and the checkboxes have no way
    // to express — or clear — a type listed twice.
    const events = isAllAlerts(formEvents) ? [] : [...new Set(formEvents)]
    // Route ORDER is meaningful — a test send without an explicit `via` uses the
    // first effective route — so editing an already-routed channel updates it in
    // place instead of removing and re-appending it. Every OTHER entry naming
    // the same via collapses into that one: a hand-written block can route a
    // via twice, and rewriting only the first match would leave behind the
    // duplicate the hub rejects, with no way to reach it from this screen.
    const routedIndex = routes.findIndex((route) => route.via === name)
    const nextRoutes = formRouted && routedIndex >= 0
      ? routes
        .filter((route, i) => route.via !== name || i === routedIndex)
        .map((route) => (route.via === name ? { via: name, events } : route))
      : routes.filter((route) => route.via !== name)
    if (formRouted && routedIndex < 0) nextRoutes.push({ via: name, events })

    const generation = saveGeneration.current
    const { persisted, message: failure } = await savePatch({ notifiers: nextNotifiers, routes: nextRoutes })
    // THIS channel's destination or routing just changed, so any test result
    // sitting on its card is about a message that went somewhere else. A
    // rejected PATCH changed nothing, and every OTHER channel's notifier and
    // route went out byte-identical, so neither is invalidated.
    if (persisted) {
      invalidateTest(name)
      setTests((current) => { const { [name]: _stale, ...rest } = current; return rest })
    }
    setSaveErrors((current) => { const { [name]: _cleared, ...rest } = current; return rest })
    setDetachedSaveError("")
    if (generation !== saveGeneration.current) {
      // The dialog this save was started from is gone, so the failure has to
      // land somewhere else. A rejected edit leaves the channel in place and
      // its card carries the reason; a rejected ADD created no notifier, so
      // there is no card and keying it by name would render the message
      // nowhere — the operator would read a silently failed add as a success.
      if (failure) {
        if (notifiers[name]) setSaveErrors((current) => ({ ...current, [name]: failure }))
        else setDetachedSaveError(`Adding channel "${name}" failed: ${failure}`)
      }
      return
    }
    if (failure) { setFormError(failure); return }
    setShowModal(false)
  }

  async function removeChannel(name: string) {
    const nextNotifiers = { ...notifiers }
    delete nextNotifiers[name]
    const generation = saveGeneration.current
    const { persisted, message: failure } = await savePatch({
      notifiers: nextNotifiers,
      routes: routes.filter((route) => route.via !== name),
    })
    if (persisted) {
      invalidateTest(name)
      setTests((current) => { const { [name]: _removed, ...rest } = current; return rest })
    }
    setSaveErrors((current) => { const { [name]: _cleared, ...rest } = current; return rest })
    if (generation !== saveGeneration.current) {
      // The channel is still there — the remove was rejected — so its card can
      // carry the reason.
      if (failure) setSaveErrors((current) => ({ ...current, [name]: failure }))
      return
    }
    if (failure) { setFormError(failure); return }
    setShowModal(false)
  }

  // Dropping a route whose notifier is gone. It has no card of its own, so this
  // is the only way back to a hub that can turn lifecycle alerts on again.
  async function removeOrphanRoute(via: string) {
    invalidateTest(via)
    setTests((current) => { const { [via]: _removed, ...rest } = current; return rest })
    // The page-level banner reports a failure here: this save is not made from
    // the dialog, so nothing covers the banner.
    await savePatch({ routes: routes.filter((route) => route.via !== via) })
  }

  // The event a test send uses: something this channel is routed for and that
  // is not muted globally, preferring the friendliest one.
  function testEventFor(name: string): string | null {
    const route = routeFor(name)
    if (!route) return null
    const allowed = route.events?.length ? knownEvents(route.events) : eventTypes
    const candidates = allowed.filter((eventType) => !isEventMuted(eventType))
    if (candidates.length === 0) return null
    return candidates.includes("agent_started") ? "agent_started" : candidates[0]
  }

  async function sendTest(name: string) {
    const eventType = testEventFor(name)
    if (!eventType) return
    // The hub bounds a test send at 30s, plenty of time for the operator to edit
    // the channel meanwhile. Landing the result afterwards would show a green
    // "sent" under a destination the message never reached.
    const generation = testStamp(name)
    setTests((current) => ({ ...current, [name]: { status: "sending", message: "" } }))
    const settle = (state: TestState) => {
      setTests((current) => {
        // A superseded result is dropped, not written — and it must leave the
        // map alone: whatever sits under this name now belongs to the newer
        // send that replaced this one (its "sending" indicator, or its real
        // error), so deleting it would erase a live result.
        if (generation !== testStamp(name)) return current
        return { ...current, [name]: state }
      })
    }
    try {
      await sendTestNotification(eventType, name)
      settle({ status: "ok", message: `Sent a "${lifecycleEventLabel(eventType)}" test alert.` })
    } catch (e) {
      settle({ status: "error", message: e instanceof Error ? e.message : "Test send failed" })
    }
  }

  // ── Scheduled reports ──────────────────────────────────────────────────────

  const [showSchedule, setShowSchedule] = useState(false)
  const [scheduleMode, setScheduleMode] = useState<"add" | "edit">("add")
  const [editScheduleId, setEditScheduleId] = useState<string | null>(null)
  const [formScheduleId, setFormScheduleId] = useState("")
  const [formReport, setFormReport] = useState(SCHEDULED_REPORTS[0].id)
  const [formVia, setFormVia] = useState<string[]>([])
  const [formAt, setFormAt] = useState("09:00")
  const [formTimezone, setFormTimezone] = useState("UTC")
  const [formWeekdays, setFormWeekdays] = useState<string[]>([])
  const [formScheduleEnabled, setFormScheduleEnabled] = useState(true)
  const [scheduleError, setScheduleError] = useState("")
  // A save made from a schedule's CARD (the enable switch) has no dialog to
  // report into — scheduleError renders inside the closed dialog — so its
  // failure lands on the card itself, keyed by schedule id.
  const [scheduleSaveErrors, setScheduleSaveErrors] = useState<Record<string, string>>({})
  const [reportTests, setReportTests] = useState<Record<string, ReportTestState>>({})
  // Invalidates an in-flight probe whose schedule has meanwhile been edited or
  // deleted: its result describes a report that went somewhere else.
  const reportTestGeneration = useRef<Record<string, number>>({})
  // Filled when the editor first opens, never during the initial render: the
  // zone list is derived from the browser, and reading it while Next is
  // prerendering would make the server and client markup disagree.
  const [zoneOptions, setZoneOptions] = useState<string[]>([])
  const loadZones = () => setZoneOptions((current) => (current.length ? current : timezoneOptions()))

  // A name the operator can leave as-is: derived from the report, and suffixed
  // only when it would collide with a schedule that already exists.
  function suggestScheduleId(report: string): string {
    const base = report.replace(/_/g, "-")
    if (!schedules.some((schedule) => schedule.id === base)) return base
    for (let i = 2; ; i++) {
      const candidate = `${base}-${i}`
      if (!schedules.some((schedule) => schedule.id === candidate)) return candidate
    }
  }

  const openAddSchedule = () => {
    loadZones()
    const report = SCHEDULED_REPORTS[0].id
    setScheduleMode("add")
    setEditScheduleId(null)
    setFormReport(report)
    setFormScheduleId(suggestScheduleId(report))
    // One channel is an unambiguous default; more than one is a choice.
    setFormVia(names.length === 1 ? [names[0]] : [])
    setFormAt("09:00")
    setFormTimezone(browserTimezone())
    setFormWeekdays([])
    setFormScheduleEnabled(true)
    setScheduleError("")
    setShowSchedule(true)
  }

  const openEditSchedule = (schedule: ScheduledNotificationView) => {
    loadZones()
    setScheduleMode("edit")
    setEditScheduleId(schedule.id)
    setFormScheduleId(schedule.id)
    setFormReport(schedule.report)
    setFormVia([...schedule.via])
    setFormAt(schedule.at)
    setFormTimezone(schedule.timezone || "UTC")
    setFormWeekdays([...schedule.weekdays])
    setFormScheduleEnabled(schedule.enabled)
    setScheduleError("")
    setShowSchedule(true)
  }

  const invalidateReportTest = (id: string) => {
    reportTestGeneration.current[id] = (reportTestGeneration.current[id] ?? 0) + 1
    setReportTests((current) => { const { [id]: _stale, ...rest } = current; return rest })
  }

  async function saveSchedule() {
    const id = formScheduleId.trim()
    if (!id) { setScheduleError("Name is required."); return }
    if (scheduleMode === "add" && schedules.some((schedule) => schedule.id === id)) {
      setScheduleError(`A report named "${id}" already exists.`)
      return
    }
    if (formVia.length === 0) { setScheduleError("Pick at least one channel to post the report to."); return }
    if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(formAt)) {
      setScheduleError("Time must be a 24-hour HH:MM value, e.g. 09:30.")
      return
    }
    setScheduleError("")

    const entry: ScheduledNotificationView = {
      id,
      report: formReport,
      via: formVia,
      at: formAt,
      timezone: formTimezone,
      weekdays: formWeekdays,
      enabled: formScheduleEnabled,
    }
    const next = editScheduleId
      ? schedules.map((schedule) => (schedule.id === editScheduleId ? entry : schedule))
      : [...schedules, entry]
    const { persisted, message } = await savePatch({ scheduled: next })
    // The schedule now posts something else, somewhere else, so any probe
    // result on its card is about a message that no longer describes it. A
    // rejected save changed nothing and keeps its result.
    if (persisted) {
      invalidateReportTest(id)
      clearScheduleSaveError(id)
    }
    if (message) { setScheduleError(message); return }
    setShowSchedule(false)
  }

  function clearScheduleSaveError(id: string) {
    setScheduleSaveErrors((current) => { const { [id]: _cleared, ...rest } = current; return rest })
  }

  async function removeSchedule(id: string) {
    const { persisted, message } = await savePatch({ scheduled: schedules.filter((schedule) => schedule.id !== id) })
    if (persisted) {
      invalidateReportTest(id)
      clearScheduleSaveError(id)
    }
    if (message) { setScheduleError(message); return }
    setShowSchedule(false)
  }

  async function toggleSchedule(schedule: ScheduledNotificationView, value: boolean) {
    const { message } = await savePatch({
      scheduled: schedules.map((other) => (other.id === schedule.id ? { ...other, enabled: value } : other)),
    })
    // A rejected toggle snaps the switch back with nothing else on screen —
    // the dialog that carries scheduleError is closed — so the failure has to
    // land on the schedule's own card.
    if (message) setScheduleSaveErrors((current) => ({ ...current, [schedule.id]: message }))
    else clearScheduleSaveError(schedule.id)
  }

  // Probes one schedule. A dry run renders the message the next due slot would
  // post without sending it; a real run posts to every channel the schedule
  // names, which is the only way to prove the whole path works. Neither
  // touches the scheduler's own state, so the real delivery still happens.
  async function runReportTest(schedule: ScheduledNotificationView, dryRun: boolean) {
    const targets = schedule.via.filter((via) => notifiers[via])
    if (targets.length === 0) return
    const generation = reportTestGeneration.current[schedule.id] ?? 0
    const settle = (state: ReportTestState) =>
      setReportTests((current) => {
        // A superseded result is dropped rather than written: whatever sits
        // under this id now belongs to the probe that replaced this one.
        if (generation !== (reportTestGeneration.current[schedule.id] ?? 0)) return current
        return { ...current, [schedule.id]: state }
      })
    setReportTests((current) => ({ ...current, [schedule.id]: { status: "sending", message: "" } }))
    try {
      if (dryRun) {
        const { empty, payload } = await sendScheduledReportTest(schedule.report, targets[0], true)
        settle(empty
          ? { status: "ok", message: "Nothing to report right now — a real run would post nothing." }
          : { status: "ok", message: `This is what would be posted to ${targets[0]}.`, payload })
        return
      }
      let empty = false
      for (const via of targets) {
        empty = (await sendScheduledReportTest(schedule.report, via, false)).empty
      }
      settle({
        status: "ok",
        message: empty
          ? "Nothing to report right now — no message was posted."
          : `Posted to ${targets.join(", ")}.`,
      })
    } catch (e) {
      settle({ status: "error", message: e instanceof Error ? e.message : "Test send failed" })
    }
  }

  const routedCount = routes.filter((route) => notifiers[route.via]).length
  // What the header badge counts: a channel is only receiving alerts if some
  // event type can actually reach it. The global category switches mute before
  // routing, so a channel routed for nothing but muted types receives nothing —
  // exactly what its own card already says, type by type.
  const receivingCount = routes.filter(
    (route) =>
      notifiers[route.via] &&
      (route.events?.length ? knownEvents(route.events) : eventTypes).some((eventType) => !isEventMuted(eventType)),
  ).length
  // Same predicate saveChannel stores by: every type checked is persisted as
  // the empty allow-list, so the summary must call it "all alerts" too — or it
  // promises a filter the save is about to discard.
  const formAllAlerts = isAllAlerts(formEvents)
  // Routing this channel off (or removing it) pauses the hub when it is the
  // last one left; buildPatch clears `enabled` rather than failing the save.
  const otherRoutedCount = routes.filter((route) => route.via !== editName && notifiers[route.via]).length

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Notifier</h2>
        <p className="text-sm text-muted-foreground mb-4">
          Send agent lifecycle alerts — starts, pull requests, stalls and failures — to Slack. Channels are hub-wide and shared by every workspace.
        </p>
        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {names.length} channel{names.length !== 1 ? "s" : ""} configured
          </span>
          {enabled ? (
            <span className="text-xs bg-green-500/10 text-green-400 border border-green-500/20 px-2 py-1 rounded font-medium">
              {receivingCount} receiving alerts
            </span>
          ) : (
            <span className="text-xs bg-amber-500/10 text-amber-400 border border-amber-500/20 px-2 py-1 rounded font-medium">
              Alerts paused
            </span>
          )}
        </div>
      </div>

      {/* Global switches: these mute an alert everywhere, before routing. */}
      <div className="border border-border rounded-lg">
        <div className="flex items-center justify-between gap-4 p-4">
          <div>
            <div className="text-sm font-medium">Lifecycle alerts</div>
            <p className="text-xs text-muted-foreground mt-0.5">
              {routedCount === 0
                ? "Add a channel and route it before turning alerts on — there is nowhere to send them yet."
                : orphanRoutes.length > 0
                  ? "Remove the routes pointing at deleted channels below — the hub refuses to enable alerts while one is left."
                  : unsupportedRoutes.length > 0
                    ? "Open Edit on the channels flagged below and save — the hub refuses to enable alerts while a route lists an unsupported alert type."
                    : duplicateRoutes.length > 0
                      ? "Open Edit on the channels flagged below and save — the hub refuses to enable alerts while a channel or an alert type is routed twice."
                      : "The master switch. Turn it off to mute every channel without losing your routing."}
            </p>
          </div>
          <Switch
            checked={enabled}
            // Gated on every shape the hub rejects when a block is enabled: no
            // routes, a route naming a notifier that no longer exists, a route
            // whose allow-list carries an alert type this hub does not support,
            // and the same `via` or the same event listed twice — all in the
            // vocabulary of hub.yaml (`via`, `events`) rather than of this
            // screen.
            disabled={saving || routedCount === 0 || orphanRoutes.length > 0 || unsupportedRoutes.length > 0 || duplicateRoutes.length > 0}
            title={
              routedCount === 0
                ? "Add a channel and route it to alerts first"
                : orphanRoutes.length > 0
                  ? "Remove the routes pointing at deleted channels first"
                  : unsupportedRoutes.length > 0
                    ? "Drop the unsupported alert types from the flagged channels first — open Edit and save"
                    : duplicateRoutes.length > 0
                      ? "Collapse the duplicated routing on the flagged channels first — open Edit and save"
                      : undefined
            }
            // Invalidated only once the hub has taken the change, exactly as
            // saveChannel/removeChannel do it: a rejected save leaves every
            // card's result describing the configuration it was sent under, and
            // wiping it here would destroy a real diagnosis (a
            // channel_not_found banner) with nothing to replace it.
            onCheckedChange={async (checked) => {
              const { persisted } = await savePatch({ enabled: checked })
              if (persisted) { testGenerationAll.current++; setTests({}) }
            }}
            aria-label="Enable lifecycle alerts"
          />
        </div>
        <div className={cn("border-t border-border p-4 space-y-3 transition-opacity", !enabled && "opacity-50")}>
          <p className="text-xs text-muted-foreground">
            Alert types muted here never reach any channel, whatever the per-channel routing says.
          </p>
          <div className="grid gap-2 sm:grid-cols-2">
            {LIFECYCLE_CATEGORIES.map((category) => (
              <div key={category.id} className="flex items-start justify-between gap-3 rounded-md border border-border/60 bg-muted/20 px-3 py-2">
                <div className="min-w-0">
                  <div className="text-sm">{category.label}</div>
                  <p className="text-xs text-muted-foreground">{category.description}</p>
                </div>
                <Switch
                  className="mt-0.5"
                  checked={categoryEnabled[category.id]}
                  disabled={saving || !enabled}
                  onCheckedChange={async (checked) => {
                    // A muted category can change what (or whether) a channel
                    // receives anything, so every test result on the page is
                    // now about a configuration that no longer exists — but
                    // only once the hub has actually taken the change. A
                    // rejected save changed nothing, and clearing the results
                    // anyway would erase a live failure diagnosis.
                    const { persisted } = await savePatch({ events: { ...categoryEnabled, [category.id]: checked } })
                    if (persisted) {
                      testGenerationAll.current++
                      setTests({})
                    }
                  }}
                  aria-label={`Toggle ${category.label} alerts`}
                />
              </div>
            ))}
          </div>
        </div>
      </div>

      {/* Channels */}
      <div className="space-y-2">
        <h3 className="text-sm font-medium">Channels</h3>
        {detachedSaveError && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
            <AlertTriangle className="size-3.5 mt-px shrink-0" />
            <span className="break-words">{detachedSaveError}</span>
          </div>
        )}
        {names.length === 0 ? (
          <p className="text-sm text-muted-foreground px-4 py-6 text-center border border-border rounded-lg">
            No channels configured. Add one to start receiving alerts in Slack.
          </p>
        ) : (
          <div className="space-y-2">
            {names.map((name) => {
              const notifier = notifiers[name]
              const route = routeFor(name)
              const test = tests[name]
              const routeUnknownEvents = unknownEvents(route?.events)
              const routeDuplicated = isDuplicated(name)
              const testEvent = testEventFor(name)
              const canTest = enabled && Boolean(testEvent) && !saving
              // Rendered as text, not as the disabled button's `title`: the
              // Button base class sets `disabled:pointer-events-none`, so a
              // native tooltip on it can never fire. The globally-muted case
              // has no badge of its own, so this is the only place the screen
              // says why nothing can reach this channel.
              const testBlockedReason = !enabled
                ? "Lifecycle alerts are turned off — this channel receives nothing."
                : !route
                  ? null // the "Not receiving alerts" badge above already says it
                  : !testEvent
                    ? "Every alert type routed here is muted by a switch above, so this channel receives nothing."
                    : null
              return (
                <div key={name} className="border border-border rounded-lg p-4">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <code className="text-sm font-mono font-medium">{name}</code>
                        <span className="text-xs text-muted-foreground capitalize">{notifier.type}</span>
                      </div>
                      <p className="text-xs text-muted-foreground mt-1">
                        <span className="font-mono">{notifier.channel || "no channel"}</span>
                        {" · "}
                        {notifier.token_secret
                          ? (
                            <>
                              token: <span className={cn("font-mono", secretMissing(notifier.token_secret) && "text-amber-400")}>{notifier.token_secret}</span>
                              {secretMissing(notifier.token_secret) && <span className="text-amber-400"> — secret not found</span>}
                            </>
                          )
                          : <span className="text-amber-400">no token secret</span>}
                      </p>
                      <div className="mt-2">
                        {!route ? (
                          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
                            Not receiving alerts
                          </span>
                        ) : isAllAlerts(route.events) ? (
                          <span className="text-xs bg-blue-500/10 text-blue-400 border border-blue-500/20 px-2 py-1 rounded font-medium">
                            All alerts
                          </span>
                        ) : (
                          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
                            {new Set(knownEvents(route.events)).size} of {eventTypes.length} alert types
                          </span>
                        )}
                      </div>
                      {routeUnknownEvents.length > 0 && (
                        <p className="text-xs text-amber-400 mt-2">
                          This route also lists {routeUnknownEvents.length === 1 ? "an alert type" : "alert types"} this hub does not support (
                          <span className="font-mono">{routeUnknownEvents.join(", ")}</span>
                          ). Lifecycle alerts cannot be turned on until {routeUnknownEvents.length === 1 ? "it is" : "they are"} gone — open Edit and save to drop {routeUnknownEvents.length === 1 ? "it" : "them"}.
                        </p>
                      )}
                      {routeDuplicated && (
                        <p className="text-xs text-amber-400 mt-2">
                          {duplicatedVias.has(name)
                            ? "This channel is routed more than once on disk — the card shows only the first entry."
                            : "This route lists the same alert type more than once on disk."}{" "}
                          Lifecycle alerts cannot be turned on until the duplicate is gone — open Edit and save to collapse it.
                        </p>
                      )}
                      {testBlockedReason && (
                        <p className="text-xs text-muted-foreground mt-2">{testBlockedReason}</p>
                      )}
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!canTest || test?.status === "sending"}
                        title={testEvent ? `Posts a sample "${lifecycleEventLabel(testEvent)}" alert` : undefined}
                        onClick={() => sendTest(name)}
                        className="gap-1.5"
                      >
                        <Send className="size-3.5" />
                        {test?.status === "sending" ? "Sending…" : "Send test"}
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => openEdit(name)}>Edit</Button>
                    </div>
                  </div>
                  {test && test.status !== "sending" && (
                    <div
                      className={cn(
                        "mt-3 flex items-start gap-2 rounded-md border px-3 py-2 text-xs",
                        test.status === "ok"
                          ? "border-green-500/20 bg-green-500/10 text-green-400"
                          : "border-destructive/30 bg-destructive/10 text-destructive",
                      )}
                    >
                      {test.status === "ok"
                        ? <CheckCircle2 className="size-3.5 mt-px shrink-0" />
                        : <AlertTriangle className="size-3.5 mt-px shrink-0" />}
                      <span className="break-words">{test.message}</span>
                    </div>
                  )}
                  {saveErrors[name] && (
                    <div className="mt-3 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                      <AlertTriangle className="size-3.5 mt-px shrink-0" />
                      <span className="break-words">Last save failed: {saveErrors[name]}</span>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </div>

      {orphanRoutes.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-sm font-medium">Routes without a channel</h3>
          <p className="text-xs text-muted-foreground">
            These alerts are routed to channels that no longer exist. The hub refuses to enable lifecycle alerts until they are removed.
          </p>
          {orphanRoutes.map((route) => (
            <div key={route.via} className="border border-amber-500/20 bg-amber-500/5 rounded-lg p-4 flex items-center justify-between gap-3">
              <div className="min-w-0">
                <code className="text-sm font-mono font-medium">{route.via}</code>
                <p className="text-xs text-amber-400 mt-1">No channel named {route.via} is configured.</p>
              </div>
              <Button
                size="sm"
                variant="ghost"
                className="text-destructive hover:text-destructive shrink-0"
                disabled={saving}
                onClick={() => removeOrphanRoute(route.via)}
              >
                <Trash2 className="size-3.5 mr-1" /> Remove route
              </Button>
            </div>
          ))}
        </div>
      )}

      <Button onClick={openAdd} className="gap-2">
        <span className="text-sm">+</span> Add Channel
      </Button>

      {/* Scheduled reports — time-driven digests, independent of the lifecycle
          alerts above: they are not routed by event type and the master switch
          does not mute them. */}
      <div className="space-y-2 border-t border-border pt-6">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h3 className="text-sm font-medium">Scheduled reports</h3>
            <p className="text-xs text-muted-foreground mt-0.5">
              Recurring digests posted at a fixed time. They run on their own schedule — the lifecycle switches above do not mute them.
            </p>
          </div>
          <Button
            size="sm"
            variant="outline"
            className="shrink-0"
            disabled={saving || names.length === 0}
            title={names.length === 0 ? "Add a channel first — a report needs somewhere to post" : undefined}
            onClick={openAddSchedule}
          >
            <span className="mr-1 text-sm">+</span> Add report
          </Button>
        </div>

        {schedules.length === 0 ? (
          <p className="text-sm text-muted-foreground px-4 py-6 text-center border border-border rounded-lg">
            {names.length === 0
              ? "No scheduled reports. Add a channel first, then schedule a report to post into it."
              : "No scheduled reports. Add one to get a recurring digest in Slack."}
          </p>
        ) : (
          <div className="space-y-2">
            {schedules.map((schedule) => {
              const test = reportTests[schedule.id]
              const missingVia = schedule.via.filter((via) => !notifiers[via])
              const targets = schedule.via.filter((via) => notifiers[via])
              const canTest = targets.length > 0 && !saving && test?.status !== "sending"
              return (
                <div key={schedule.id} className="border border-border rounded-lg p-4">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="text-sm font-medium">{scheduledReportLabel(schedule.report)}</span>
                        <code className="text-xs text-muted-foreground font-mono">{schedule.id}</code>
                        {!schedule.enabled && (
                          <span className="text-xs bg-amber-500/10 text-amber-400 border border-amber-500/20 px-2 py-0.5 rounded font-medium">
                            Paused
                          </span>
                        )}
                      </div>
                      <p className="text-xs text-muted-foreground mt-1 flex items-center gap-1.5">
                        <Clock className="size-3 shrink-0" />
                        <span className="font-mono">{schedule.at}</span>
                        <span>{schedule.timezone || "UTC"}</span>
                        <span>·</span>
                        <span>{weekdaysLabel(schedule.weekdays)}</span>
                      </p>
                      <p className="text-xs text-muted-foreground mt-1">
                        Posts to{" "}
                        {schedule.via.map((via, i) => (
                          <React.Fragment key={via}>
                            {i > 0 && ", "}
                            <span className={cn("font-mono", !notifiers[via] && "text-amber-400")}>{via}</span>
                          </React.Fragment>
                        ))}
                      </p>
                      {missingVia.length > 0 && (
                        <p className="text-xs text-amber-400 mt-2">
                          {missingVia.length === 1 ? "Channel" : "Channels"}{" "}
                          <span className="font-mono">{missingVia.join(", ")}</span>{" "}
                          {missingVia.length === 1 ? "is" : "are"} no longer configured, so nothing is delivered there — open Edit and pick another.
                        </p>
                      )}
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      <Switch
                        checked={schedule.enabled}
                        disabled={saving}
                        onCheckedChange={(checked) => toggleSchedule(schedule, checked)}
                        aria-label={`Enable the ${schedule.id} report`}
                      />
                      <Button size="sm" variant="ghost" onClick={() => openEditSchedule(schedule)}>Edit</Button>
                    </div>
                  </div>
                  <div className="flex items-center gap-2 mt-3">
                    <Button
                      size="sm"
                      variant="outline"
                      className="gap-1.5"
                      disabled={!canTest}
                      title="Renders the report without posting it"
                      onClick={() => runReportTest(schedule, true)}
                    >
                      <Eye className="size-3.5" />
                      Preview
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      className="gap-1.5"
                      disabled={!canTest}
                      title={targets.length > 0 ? `Posts the report now to ${targets.join(", ")}` : undefined}
                      onClick={() => runReportTest(schedule, false)}
                    >
                      <Send className="size-3.5" />
                      {test?.status === "sending" ? "Working…" : "Send now"}
                    </Button>
                    {targets.length === 0 && (
                      <span className="text-xs text-muted-foreground">
                        No configured channel to post to.
                      </span>
                    )}
                  </div>
                  {test && test.status !== "sending" && (
                    <div
                      className={cn(
                        "mt-3 rounded-md border px-3 py-2 text-xs",
                        test.status === "ok"
                          ? "border-green-500/20 bg-green-500/10 text-green-400"
                          : "border-destructive/30 bg-destructive/10 text-destructive",
                      )}
                    >
                      <div className="flex items-start gap-2">
                        {test.status === "ok"
                          ? <CheckCircle2 className="size-3.5 mt-px shrink-0" />
                          : <AlertTriangle className="size-3.5 mt-px shrink-0" />}
                        <span className="break-words">{test.message}</span>
                      </div>
                      {test.payload !== undefined && (
                        <pre className="mt-2 max-h-64 overflow-auto rounded bg-background/60 p-2 text-xs text-foreground whitespace-pre-wrap break-words">
                          {previewLines(test.payload).join("\n\n")}
                        </pre>
                      )}
                    </div>
                  )}
                  {scheduleSaveErrors[schedule.id] && (
                    <div className="mt-3 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                      <AlertTriangle className="size-3.5 mt-px shrink-0" />
                      <span className="break-words">Last save failed: {scheduleSaveErrors[schedule.id]}</span>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </div>

      {/* Add / edit modal */}
      {/* Closing only closes: DialogContent stays mounted for its 200ms exit
          animation, so clearing the form here would let the operator watch the
          heading turn into "Edit null" and every field blank out on the way
          out. openAdd/openEdit re-seed the whole form, so nothing needs
          clearing on close. */}
      <Dialog open={showModal} onOpenChange={setShowModal}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">{modalMode === "add" ? "Add Channel" : `Edit ${editName}`}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{modalMode === "add" ? "Add Channel" : `Edit ${editName}`}</h3>
          </div>

          <div className="p-5 space-y-4">
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                <Input
                  placeholder="e.g. eng-agents"
                  value={formName}
                  onChange={(e) => setFormName(e.target.value)}
                  className="font-mono text-sm h-8"
                  disabled={saving || modalMode === "edit"}
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Slack channel ID</label>
                {/* Every control below is disabled while this dialog's own save
                    is in flight: saveChannel snapshots the form at click time
                    and closes the dialog on success, so an edit made during the
                    round trip — seconds long, the re-read hits GitHub — would be
                    discarded with nothing on screen saying so. */}
                <Input
                  placeholder="C0123ABCD"
                  value={formChannel}
                  onChange={(e) => setFormChannel(e.target.value)}
                  className="font-mono text-sm h-8"
                  disabled={saving}
                />
              </div>
            </div>
            <p className="text-xs text-muted-foreground -mt-2">
              Use the channel ID, not the #name — names break when a channel is renamed. In Slack, open the channel details and copy the ID at the bottom.
            </p>

            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Bot token secret</label>
              <select
                value={formTokenSecret}
                onChange={(e) => setFormTokenSecret(e.target.value)}
                className="w-full h-8 text-sm rounded-md border border-input bg-background px-3"
                disabled={saving}
              >
                <option value="">Select secret…</option>
                {/* A dangling reference has no option of its own, so the
                    controlled select would render blank while Save happily
                    writes the broken name back. Give it one, flagged. */}
                {secretMissing(formTokenSecret) && (
                  <option value={formTokenSecret}>{formTokenSecret} (secret not found)</option>
                )}
                {secretNames.map((secret) => (
                  <option key={secret} value={secret}>{secret}</option>
                ))}
              </select>
              {secretMissing(formTokenSecret) ? (
                <p className="text-xs text-amber-400 mt-1">
                  No hub secret named <span className="font-mono">{formTokenSecret}</span> exists, so sends through this channel fail. Add it under Settings → Secrets → Hub, or pick another.
                </p>
              ) : (
                <p className="text-xs text-muted-foreground mt-1">
                  {/* Channels are hub-wide, so the token must be a HUB secret —
                      a workspace secret of the same name is a different store
                      and never resolves here. */}
                  The <strong>hub</strong> secret holding the Slack bot token (<span className="font-mono">xoxb-…</span>). Add it under Settings → Secrets → Hub first — workspace secrets are a separate store and are never read here.
                </p>
              )}
            </div>

            {/* The hub rejects a channel whose interval it cannot parse, on
                every save that touches it. Editable here so a value written by
                hand can be repaired from the screen instead of hub.yaml. */}
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Minimum send interval</label>
              <Input
                placeholder="30s"
                value={formMinSendInterval}
                onChange={(e) => setFormMinSendInterval(e.target.value)}
                className="font-mono text-sm h-8"
                disabled={saving}
              />
              <p className="text-xs text-muted-foreground mt-1">
                Optional. How long this channel waits between messages, as a duration (<span className="font-mono">30s</span>, <span className="font-mono">5m</span>). Leave empty for the default.
              </p>
            </div>

            {/* Routing */}
            <div className="space-y-3 border-t border-border pt-4">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <div className="text-sm font-medium">Alert routing</div>
                  <p className="text-xs text-muted-foreground mt-0.5">Which lifecycle alerts land in this channel.</p>
                </div>
                <Switch
                  className="mt-1"
                  checked={formRouted}
                  onCheckedChange={setFormRouted}
                  disabled={saving}
                  aria-label="Send lifecycle alerts to this channel"
                />
              </div>

              {modalMode === "edit" && enabled && otherRoutedCount === 0 && (
                <p className="text-xs text-amber-400 rounded-md border border-amber-500/20 bg-amber-500/10 px-3 py-2">
                  This is the only channel receiving alerts. Removing it — or turning routing off — pauses lifecycle alerts for the whole hub until another channel is routed.
                </p>
              )}

              {!formRouted ? (
                <p className="text-xs text-muted-foreground rounded-md border border-border bg-muted/20 px-3 py-2">
                  This channel stays configured but receives no lifecycle alerts.
                </p>
              ) : (
                <>
                  <div
                    className={cn(
                      "rounded-md border px-3 py-2",
                      formAllAlerts
                        ? "border-blue-500/30 bg-blue-500/10"
                        : "border-border bg-muted/20",
                    )}
                  >
                    <div className="flex items-start gap-2">
                      <Bell className={cn("size-4 mt-px shrink-0", formAllAlerts ? "text-blue-400" : "text-muted-foreground")} />
                      <div className="min-w-0">
                        <div className={cn("text-sm font-medium", formAllAlerts ? "text-blue-400" : "")}>
                          {formAllAlerts ? "All alerts" : `${formEvents.length} of ${eventTypes.length} alert types`}
                        </div>
                        <p className="text-xs text-muted-foreground">
                          {formAllAlerts
                            ? "Saved as receive-all: this channel gets every alert type, including any added later."
                            : "Only the checked types reach this channel."}
                        </p>
                        {!formAllAlerts && (
                          <button
                            type="button"
                            className="text-xs text-blue-400 hover:underline mt-1 disabled:opacity-50"
                            disabled={saving}
                            onClick={() => setFormEvents([])}
                          >
                            Clear selection to receive all alerts
                          </button>
                        )}
                      </div>
                    </div>
                  </div>

                  {formDroppedEvents.length > 0 && (
                    <p className="text-xs text-amber-400 rounded-md border border-amber-500/20 bg-amber-500/10 px-3 py-2">
                      This route also lists {formDroppedEvents.length === 1 ? "an alert type" : "alert types"} this hub does not support (
                      <span className="font-mono">{formDroppedEvents.join(", ")}</span>
                      ), so {formDroppedEvents.length === 1 ? "it has" : "they have"} no checkbox below and nothing is ever delivered for {formDroppedEvents.length === 1 ? "it" : "them"}. Saving drops {formDroppedEvents.length === 1 ? "it" : "them"} — the hub refuses to enable lifecycle alerts until then.
                    </p>
                  )}

                  <div className="space-y-1">
                    {eventTypes.map((eventType) => {
                      const checked = formEvents.includes(eventType)
                      const muted = isEventMuted(eventType)
                      return (
                        <label
                          key={eventType}
                          className="flex items-center gap-2 text-sm cursor-pointer rounded px-2 py-1 hover:bg-muted/50"
                        >
                          <input
                            type="checkbox"
                            aria-label={`${lifecycleEventLabel(eventType)} (${eventType})`}
                            checked={checked}
                            disabled={saving}
                            onChange={(e) =>
                              setFormEvents(
                                e.target.checked
                                  ? [...formEvents, eventType]
                                  : formEvents.filter((t) => t !== eventType),
                              )
                            }
                          />
                          <span className={cn(!checked && !formAllAlerts && "text-muted-foreground")}>
                            {lifecycleEventLabel(eventType)}
                          </span>
                          <code className="text-xs text-muted-foreground font-mono">{eventType}</code>
                          {muted && (
                            <span className="text-xs text-amber-400 ml-auto shrink-0">muted globally</span>
                          )}
                        </label>
                      )
                    })}
                  </div>
                </>
              )}
            </div>

          </div>

          <div className="border-t border-border">
            {/* The error sits with the buttons, never below the fold of a long
                scrolling form. */}
            {/* A failure from the save that THIS dialog replaced. The channel
                list behind the overlay carries it too, but the operator is
                looking at the dialog — and its message names the channel, so
                it cannot be mistaken for a rejection of the form on screen. */}
            {detachedSaveError && (
              <div className="mx-5 mt-4 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                <AlertTriangle className="size-3.5 mt-px shrink-0" />
                <span className="break-words">{detachedSaveError}</span>
              </div>
            )}
            {formError && (
              <div className="mx-5 mt-4 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                <AlertTriangle className="size-3.5 mt-px shrink-0" />
                <span className="break-words">{formError}</span>
              </div>
            )}
            <div className="flex items-center justify-between px-5 py-4">
              {modalMode === "edit" && editName && (
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive hover:text-destructive"
                  disabled={saving}
                  onClick={() => removeChannel(editName)}
                >
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" disabled={saving} onClick={() => setShowModal(false)}>Cancel</Button>
                <Button size="sm" disabled={saving || !formName.trim() || !formChannel.trim() || !formTokenSecret} onClick={saveChannel}>
                  {modalMode === "add" ? "Add Channel" : "Save changes"}
                </Button>
              </div>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* Add / edit scheduled report. Like the channel dialog, closing only
          closes: openAddSchedule/openEditSchedule re-seed every field. */}
      <Dialog open={showSchedule} onOpenChange={setShowSchedule}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">
            {scheduleMode === "add" ? "Add scheduled report" : `Edit ${editScheduleId}`}
          </DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{scheduleMode === "add" ? "Add scheduled report" : `Edit ${editScheduleId}`}</h3>
          </div>

          <div className="p-5 space-y-4">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Report</label>
              <select
                value={formReport}
                onChange={(e) => setFormReport(e.target.value)}
                className="w-full h-8 text-sm rounded-md border border-input bg-background px-3"
                disabled={saving}
              >
                {/* A report this build does not know can only come from a
                    hand-written hub.yaml. It gets an option of its own, flagged,
                    so the controlled select cannot render blank while Save
                    writes the unknown name straight back. */}
                {!SCHEDULED_REPORTS.some((report) => report.id === formReport) && (
                  <option value={formReport}>{formReport} (not supported by this hub)</option>
                )}
                {SCHEDULED_REPORTS.map((report) => (
                  <option key={report.id} value={report.id}>{report.label}</option>
                ))}
              </select>
              <p className="text-xs text-muted-foreground mt-1">
                {SCHEDULED_REPORTS.find((report) => report.id === formReport)?.description
                  || "This hub does not carry this report, so the schedule delivers nothing."}
              </p>
            </div>

            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Name</label>
              <Input
                placeholder="e.g. pending-prs"
                value={formScheduleId}
                onChange={(e) => setFormScheduleId(e.target.value)}
                className="font-mono text-sm h-8"
                disabled={saving || scheduleMode === "edit"}
              />
              <p className="text-xs text-muted-foreground mt-1">
                How this schedule is identified in <span className="font-mono">hub.yaml</span> and in the hub log. It cannot be changed later.
              </p>
            </div>

            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Channels</label>
              {names.length === 0 && formVia.length === 0 ? (
                <p className="text-xs text-amber-400">No channels are configured yet.</p>
              ) : (
                <div className="space-y-1">
                  {/* Deleted channels the schedule still names get a checkbox
                      of their own, flagged: the hub refuses to save a schedule
                      pointing at one, so unticking it has to be possible from
                      here — otherwise the only repair is a hub.yaml edit. */}
                  {[...names, ...formVia.filter((via) => !notifiers[via])].map((name) => {
                    const missing = !notifiers[name]
                    return (
                      <label
                        key={name}
                        className="flex items-center gap-2 text-sm cursor-pointer rounded px-2 py-1 hover:bg-muted/50"
                      >
                        <input
                          type="checkbox"
                          aria-label={`Post the report to ${name}`}
                          checked={formVia.includes(name)}
                          disabled={saving}
                          onChange={(e) =>
                            setFormVia(
                              e.target.checked
                                ? [...formVia, name]
                                : formVia.filter((via) => via !== name),
                            )
                          }
                        />
                        <code className={cn("font-mono", missing && "text-amber-400")}>{name}</code>
                        <span className={cn("text-xs", missing ? "text-amber-400" : "text-muted-foreground")}>
                          {missing ? "channel no longer configured" : notifiers[name]?.channel}
                        </span>
                      </label>
                    )
                  })}
                </div>
              )}
              {formVia.some((via) => !notifiers[via]) && (
                <p className="text-xs text-amber-400 mt-1">
                  The hub refuses to save a report that posts to a channel it does not have. Untick the flagged one, or add the channel back first.
                </p>
              )}
            </div>

            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Time</label>
                <Input
                  type="time"
                  value={formAt}
                  onChange={(e) => setFormAt(e.target.value)}
                  className="font-mono text-sm h-8"
                  disabled={saving}
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Timezone</label>
                <select
                  value={formTimezone}
                  onChange={(e) => setFormTimezone(e.target.value)}
                  className="w-full h-8 text-sm rounded-md border border-input bg-background px-3"
                  disabled={saving}
                >
                  {/* A zone this browser does not list — an older browser, or a
                      name written by hand — still needs an option, or the
                      select renders blank and Save rewrites it. */}
                  {formTimezone && !zoneOptions.includes(formTimezone) && (
                    <option value={formTimezone}>{formTimezone}</option>
                  )}
                  {zoneOptions.map((zone) => (
                    <option key={zone} value={zone}>{zone}</option>
                  ))}
                </select>
              </div>
            </div>

            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Days</label>
              <div className="flex flex-wrap gap-1.5">
                {WEEKDAYS.map((day) => {
                  const selected = formWeekdays.includes(day.id)
                  return (
                    <button
                      key={day.id}
                      type="button"
                      aria-pressed={selected}
                      disabled={saving}
                      onClick={() =>
                        setFormWeekdays(
                          selected
                            ? formWeekdays.filter((weekday) => weekday !== day.id)
                            : [...formWeekdays, day.id],
                        )
                      }
                      className={cn(
                        "px-2.5 py-1 text-xs rounded-md border transition-colors disabled:opacity-50",
                        selected
                          ? "border-blue-500/30 bg-blue-500/10 text-blue-400"
                          : "border-border bg-muted/20 text-muted-foreground hover:bg-muted/50",
                      )}
                    >
                      {day.label}
                    </button>
                  )
                })}
              </div>
              <p className="text-xs text-muted-foreground mt-1">
                {formWeekdays.length === 0
                  ? "No days selected — runs every day."
                  : `Runs on ${weekdaysLabel(formWeekdays)}.`}
              </p>
            </div>

            <div className="flex items-start justify-between gap-3 border-t border-border pt-4">
              <div>
                <div className="text-sm font-medium">Enabled</div>
                <p className="text-xs text-muted-foreground mt-0.5">
                  Turn it off to keep the schedule without posting anything.
                </p>
              </div>
              <Switch
                className="mt-1"
                checked={formScheduleEnabled}
                onCheckedChange={setFormScheduleEnabled}
                disabled={saving}
                aria-label="Enable this scheduled report"
              />
            </div>
          </div>

          <div className="border-t border-border">
            {scheduleError && (
              <div className="mx-5 mt-4 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                <AlertTriangle className="size-3.5 mt-px shrink-0" />
                <span className="break-words">{scheduleError}</span>
              </div>
            )}
            <div className="flex items-center justify-between px-5 py-4">
              {scheduleMode === "edit" && editScheduleId && (
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive hover:text-destructive"
                  disabled={saving}
                  onClick={() => removeSchedule(editScheduleId)}
                >
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" disabled={saving} onClick={() => setShowSchedule(false)}>Cancel</Button>
                <Button
                  size="sm"
                  disabled={saving || !formScheduleId.trim() || formVia.length === 0 || !formAt}
                  onClick={saveSchedule}
                >
                  {scheduleMode === "add" ? "Add report" : "Save changes"}
                </Button>
              </div>
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

  // Only called from the mount effect, where error is already null — so no
  // state needs to be set before the fetch resolves.
  const load = useCallback((refresh = false) => {
    const hubUrl = getHubUrl()
    const token = getAuthToken() || ""
    return fetch(`${hubUrl}/api/doctor${refresh ? "?refresh=true" : ""}`, {
      headers: { Authorization: `Bearer ${token}` },
    })
      .then(async (res) => {
        if (!res.ok) throw new Error(await res.text())
        return res.json()
      })
      .then((data) => {
        setReport(data)
        setError(null)
      })
      .catch((e: any) => {
        setError(e.message || "Failed to load diagnostics")
      })
      .finally(() => setLoading(false))
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
