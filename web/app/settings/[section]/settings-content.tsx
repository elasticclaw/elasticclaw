"use client"

import { useParams, useRouter } from "next/navigation"
import React, { useEffect, useState, useCallback, useRef } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Factory, Copy, Check, LayoutTemplate, Trash2, Lock, Sparkles, Send, RotateCcw, Eye, EyeOff, ExternalLink, AlertTriangle, X, CheckCircle2, Webhook, Stethoscope, ArrowRight, BarChart3, Wrench } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import Link from "next/link"
import { VALID_SECTIONS, type Section } from "./sections"

function isValidSection(s: string): s is Section {
  return VALID_SECTIONS.includes(s as Section)
}

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
  factories?: Array<{
    name: string; integration: string; workspace: string; team: string
    triggerStatus: string; doneStatus: string; terminateOnLeave: boolean; template: string
    webhookSecretSet?: boolean; tags?: string[]; color?: string; enabled?: boolean; labels?: string[]; assigned_to?: string
    concurrencyGroup?: string
    inputs?: Array<{
      name: string; type: string; required?: boolean; default?: string
      description?: string; options?: string[]; validation?: string
    }>
  }>
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
  const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error("Failed to load settings")
  return res.json()
}

async function patchSettings(patch: object): Promise<void> {
  const hubUrl = getHubUrl()
  const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    method: "PATCH",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  })
  if (!res.ok) throw new Error(await res.text())
}

export default function SettingsSectionPage() {
  const params = useParams()
  const router = useRouter()
  const rawSection = Array.isArray(params.section) ? params.section[0] : (params.section ?? "runtimes")
  const section: Section = isValidSection(rawSection) ? rawSection : "runtimes"

  // Redirect invalid sections to runtimes
  useEffect(() => {
    if (!isValidSection(rawSection)) {
      router.replace("/settings/runtimes")
    }
  }, [rawSection, router])

  const [settings, setSettings] = useState<SettingsData | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  const [success, setSuccess] = useState("")
  const [version, setVersion] = useState("")
  const [hubPublicUrl, setHubPublicUrl] = useState("")

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
    const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
    fetch(`${hubUrl}/api/hub-config`, { headers: { Authorization: `Bearer ${token}` } })
      .then((r) => r.json())
      .then((d) => { setVersion(d.version || "unknown"); setHubPublicUrl(d.hubUrl || "") })
      .catch(() => {})
  }, [])

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

  const navGroups: { id: Section; label: string; icon: React.ElementType }[][] = [
    // Infrastructure
    [
      { id: "runtimes", label: "Sandboxes", icon: Cpu },
      { id: "models", label: "Models", icon: Key },
    ],
    // Integrations
    [
      { id: "github", label: "GitHub Apps", icon: Github },
      { id: "issue-trackers", label: "Issue Trackers", icon: Zap },
      { id: "mcp-servers", label: "MCP Servers", icon: Zap },
      { id: "webhooks", label: "Webhooks", icon: Webhook },
    ],
    // Configuration
    [
      { id: "secrets", label: "Secrets", icon: Lock },
      { id: "templates", label: "Templates", icon: LayoutTemplate },
      { id: "factories", label: "Factories", icon: Factory },
    ],
    // Access
    [
      { id: "authentication", label: "Authentication", icon: Shield },
    ],
    // AI Assistant
    [
      { id: "ai-config", label: "Configure with AI", icon: Sparkles },
    ],
    // Analytics
    [
      { id: "analytics", label: "Analytics", icon: BarChart3 },
    ],
    // Diagnostics
    [
      { id: "doctor", label: "Doctor", icon: Stethoscope },
      { id: "troubleshoot", label: "Troubleshoot", icon: Wrench },
    ],
  ]

  return (
    <div className="h-screen bg-background flex flex-col overflow-hidden">
      {/* Header */}
      <header className="border-b border-border px-6 py-4 flex items-center gap-4">
        <Button variant="ghost" size="icon" onClick={() => window.location.href = "/"}>
          <ChevronLeft className="size-4" />
        </Button>
        <h1 className="text-lg font-semibold">Settings</h1>
        {version && <span className="ml-auto text-xs text-muted-foreground font-mono">{version}</span>}
      </header>

      <div className="flex flex-1 min-h-0 overflow-hidden">
        {/* Left nav */}
        <aside className="w-56 border-r border-border p-4 flex flex-col overflow-y-auto">
          <div className="space-y-1 flex-1">
            {navGroups.map((group, groupIdx) => (
              <div key={groupIdx}>
                {groupIdx > 0 && <div className="my-2 border-t border-border/50" />}
                {group.map(({ id, label, icon: Icon }) => (
                  <Link
                    key={id}
                    href={`/settings/${id}`}
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
            <GitHubSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "authentication" && (
            <AuthenticationSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "issue-trackers" && (
            <IntegrationsSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "factories" && (
            <FactoriesSection hubUrl={hubPublicUrl} settings={settings} onSave={save} onSaveSilent={saveSilent} saving={saving} />
          )}
          {section === "secrets" && (
            <SecretsSection settings={settings} />
          )}
          {settings && section === "mcp-servers" && (
            <MCPServersSection settings={settings} onSave={save} saving={saving} />
          )}
          {section === "templates" && (
            <TemplatesSection />
          )}
          {section === "ai-config" && (
            <AIConfigSection />
          )}
          {section === "webhooks" && (
            <WebhooksSection hubUrl={hubPublicUrl} />
          )}
          {section === "doctor" && (
            <DoctorSection />
          )}
          {section === "troubleshoot" && (
            <TroubleshootSection />
          )}
          {section === "analytics" && (
            <AnalyticsSection />
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
  { value: "codex",      label: "Codex",      placeholder: "sk-proj-..." },
  { value: "other",      label: "Other",      placeholder: "" },
]

const PROVIDER_MODELS: Record<string, { id: string; name: string }[]> = {
  anthropic: [
    { id: "anthropic/claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    { id: "anthropic/claude-opus-4-5",   name: "Claude Opus 4.5" },
    { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
  ],
  fireworks: [
    { id: "fireworks/accounts/fireworks/models/kimi-k2p6",                  name: "Kimi K2.6" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-pro",            name: "DeepSeek V4 Pro" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-flash",          name: "DeepSeek V4 Flash" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p7",               name: "MiniMax M2.7" },
    { id: "fireworks/accounts/fireworks/models/glm-5p1",                    name: "GLM 5.1" },
    { id: "fireworks/accounts/fireworks/models/qwen3p6-plus",               name: "Qwen3.6 Plus" },
    { id: "fireworks/accounts/fireworks/models/kimi-k2p5",                  name: "Kimi K2.5" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-120b",               name: "OpenAI gpt-oss-120b" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-20b",                name: "OpenAI gpt-oss-20b" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p5",               name: "MiniMax M2.5" },
    { id: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct",    name: "Llama 3.3 70B Instruct" },
  ],
  codex: [
    { id: "codex/o4-mini", name: "Codex o4-mini" },
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

  const providerLabel = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.label ?? p
  const providerPlaceholder = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.placeholder ?? ""

  const resetForm = () => {
    setFormName(""); setFormProvider("anthropic"); setFormCustomProvider(""); setFormKey(""); setFormDefault(false); setFormDefaultModel(""); setEditIdx(null)
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
    setFormDefaultModel(k.defaultModel || "")
    setEditIdx(i)
    setModalMode("edit")
    setShowModal(true)
  }

  const needsAttention = llmKeys.filter(k => !k.keySet).length
  const configuredCount = llmKeys.filter(k => k.keySet).length

  function doSave() {
    const actualProvider = formProvider === "other" ? formCustomProvider : formProvider
    if (modalMode === "add") {
      if (!formName.trim() || !formKey.trim()) return
      onSave({ llmKeys: [{ name: formName.trim(), provider: actualProvider, apiKey: formKey.trim(), default: formDefault, defaultModel: formDefaultModel || undefined }] })
    } else if (editIdx !== null) {
      const existing = llmKeys[editIdx]
      const patch: Record<string, unknown> = {
        name: existing.name,          // lookup key — original name
        newName: formName.trim(),      // new name if changed
      }
      if (formKey.trim()) patch.apiKey = formKey.trim()
      patch.default = formDefault       // always send so user can unset
      if (formDefaultModel) patch.defaultModel = formDefaultModel
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
          API keys for LLM providers. The default key is used unless overridden by a template.
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
                    {k.provider === "anthropic" || k.provider === "codex" ? (
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
                    onChange={e => { setFormProvider(e.target.value); setFormDefaultModel("") }}
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

function GitHubSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const [showModal, setShowModal] = useState(false)
  const [appId, setAppId] = useState("")
  const [url, setUrl] = useState("")
  const [pem, setPem] = useState("")
  const [testResult, setTestResult] = useState<GitHubAppView | null>(null)
  const [testing, setTesting] = useState(false)
  const [testError, setTestError] = useState("")

  const resetModal = () => {
    setAppId(""); setUrl(""); setPem("")
    setTestResult(null); setTestError(""); setTesting(false)
  }

  const openModal = () => { resetModal(); setShowModal(true) }
  const closeModal = () => { setShowModal(false); resetModal() }

  async function runTest() {
    setTesting(true); setTestError(""); setTestResult(null)
    try {
      const hubUrl = getHubUrl()
      const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
      const res = await fetch(`${hubUrl}/api/settings/github/test`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
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

  function doSave(force = false) {
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
    const newApp: { appId: number; privateKeyPem: string; url?: string } = { appId: parsedAppId, privateKeyPem: pem }
    if (url) newApp.url = url
    onSave({ github: [...(settings.github || []), newApp] })
    closeModal()
  }

  const needsAttention = testResult?.permissions?.filter(p => !p.ok).length ?? 0
  const configuredCount = testResult?.permissions?.filter(p => p.ok).length ?? 0
  const totalCount = testResult?.permissions?.length ?? 0

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">GitHub Apps</h2>
      <div className="text-sm text-muted-foreground mb-6 space-y-1.5">
        <p>
          Register a GitHub App so your ElasticClaw templates can access repositories.
          When a claw is created, it gets a scoped token that can read and write code,
          open pull requests, and check CI status — but only on repos the App is installed on.
        </p>
        <p>
          The App needs <strong>contents:write</strong>, <strong>pull_requests:write</strong>,
          and read access to <strong>metadata</strong>, <strong>checks</strong>, and <strong>statuses</strong>.
          Install it on your org or specific repos, then add the App ID and private key here.
        </p>
      </div>

      {settings.github?.length > 0 && (
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
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App ID</label>
                  <Input type="number" value={appId} onChange={e => setAppId(e.target.value)} className="h-8 text-sm" placeholder="123456" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App URL (optional)</label>
                  <Input value={url} onChange={e => setUrl(e.target.value)} className="h-8 text-sm" placeholder="https://github.com/apps/..." />
                </div>
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

  // Password card state
  const [newPw, setNewPw] = useState('')
  const [pwConfirm, setPwConfirm] = useState('')
  const [pwErr, setPwErr] = useState('')

  // GitHub OAuth config state
  const [showGhForm, setShowGhForm] = useState(false)
  const [clientId, setClientId] = useState(ghOAuth?.clientId || '')
  const [clientSecret, setClientSecret] = useState('')
  const [allowedUsers, setAllowedUsers] = useState((ghOAuth?.allowedUsers || []).join(', '))
  const [allowedOrgs, setAllowedOrgs] = useState((ghOAuth?.allowedOrgs || []).join(', '))
  const [allowedTeams, setAllowedTeams] = useState((ghOAuth?.allowedTeams || []).join(', '))
  const [admins, setAdmins] = useState((ghAccess?.admins || []).join(', '))
  const [viewTags, setViewTags] = useState((ghAccess?.viewRequiresTags || []).join(', '))
  const [interactTags, setInteractTags] = useState((ghAccess?.interactRequiresTags || []).join(', '))
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
    setAllowedUsers((ghOAuth?.allowedUsers || []).join(', '))
    setAllowedOrgs((ghOAuth?.allowedOrgs || []).join(', '))
    setAllowedTeams((ghOAuth?.allowedTeams || []).join(', '))
    setAdmins((ghAccess?.admins || []).join(', '))
    setViewTags((ghAccess?.viewRequiresTags || []).join(', '))
    setInteractTags((ghAccess?.interactRequiresTags || []).join(', '))
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
            {ghAccess?.admins && ghAccess.admins.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Admins</span><span>{ghAccess.admins.join(', ')}</span></div>}
            {ghOAuth.allowedUsers.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed users</span><span>{ghOAuth.allowedUsers.join(', ')}</span></div>}
            {ghOAuth.allowedOrgs.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed orgs</span><span>{ghOAuth.allowedOrgs.join(', ')}</span></div>}
            {ghOAuth.allowedTeams.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed teams</span><span>{ghOAuth.allowedTeams.join(', ')}</span></div>}
            {ghAccess?.viewRequiresTags && ghAccess.viewRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">View tag filter</span><span className="font-mono">{ghAccess.viewRequiresTags.join(', ')}</span></div>}
            {ghAccess?.interactRequiresTags && ghAccess.interactRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Interact tag filter</span><span className="font-mono">{ghAccess.interactRequiresTags.join(', ')}</span></div>}
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

function IntegrationsSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const linear = settings.integrations?.linear || []
  const shortcut = settings.integrations?.shortcut || []
  const githubIssues = settings.integrations?.githubIssues || []

  const allTrackers: TrackerItem[] = [
    ...linear.map((li: { workspace: string; tokenSet?: boolean }) => ({ type: "linear" as TrackerType, workspace: li.workspace, tokenSet: li.tokenSet ?? false })),
    ...shortcut.map((sc: { workspace: string; tokenSet?: boolean }) => ({ type: "shortcut" as TrackerType, workspace: sc.workspace, tokenSet: sc.tokenSet ?? false })),
    ...githubIssues.map((gi: { workspace: string; tokenSet?: boolean }) => ({ type: "github-issues" as TrackerType, workspace: gi.workspace, tokenSet: gi.tokenSet ?? false })),
  ]

  // Unified modal state
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [modalType, setModalType] = useState<TrackerType>("linear")
  const [editIdx, setEditIdx] = useState<number | null>(null)
  const [editType, setEditType] = useState<TrackerType>("linear")
  const [workspace, setWorkspace] = useState("")
  const [token, setToken] = useState("")
  const [webhookSecret, setWebhookSecret] = useState("")
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
    setWorkspace(""); setToken(""); setWebhookSecret(""); setEditIdx(null); setEditType("linear")
  }

  const openAdd = (type: TrackerType) => {
    resetModal()
    setModalType(type)
    setModalMode("add")
    setShowModal(true)
    setShowAddMenu(false)
  }

  const openEdit = (tracker: TrackerItem, idx: number) => {
    let typeIdx: number
    if (tracker.type === "linear") {
      typeIdx = idx
    } else if (tracker.type === "shortcut") {
      typeIdx = idx - linear.length
    } else {
      typeIdx = idx - linear.length - shortcut.length
    }
    setWorkspace(tracker.workspace)
    setToken("")
    setWebhookSecret("")
    setEditIdx(typeIdx)
    setEditType(tracker.type)
    setModalMode("edit")
    setShowModal(true)
  }

  function saveTracker() {
    if (!workspace.trim()) return
    if (modalMode === "add" && !token.trim()) return

    const type = modalMode === "add" ? modalType : editType

    if (type === "linear") {
      if (modalMode === "add") {
        onSave({ integrations: { linear: [...linear, { workspace: workspace.trim(), token: token.trim(), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }] } })
      } else if (editIdx !== null) {
        const li = linear[editIdx]
        const updated = linear.map((item: { workspace: string }, i: number) => i === editIdx
          ? { workspace: workspace.trim(), originalWorkspace: li.workspace, ...(token.trim() ? { token: token.trim() } : {}), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }
          : { workspace: item.workspace }
        )
        onSave({ integrations: { linear: updated } })
      }
    } else if (type === "shortcut") {
      if (modalMode === "add") {
        onSave({ integrations: { shortcut: [...shortcut.map((x: { workspace: string }) => ({ workspace: x.workspace })), { workspace: workspace.trim(), token: token.trim(), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }] } })
      } else if (editIdx !== null) {
        const sc = shortcut[editIdx]
        const updated = shortcut.map((item: { workspace: string }, i: number) => i === editIdx
          ? { workspace: workspace.trim(), originalWorkspace: sc.workspace, ...(token.trim() ? { token: token.trim() } : {}), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }
          : { workspace: item.workspace }
        )
        onSave({ integrations: { shortcut: updated } })
      }
    } else {
      // github-issues
      if (modalMode === "add") {
        onSave({ integrations: { githubIssues: [...githubIssues.map((x: { workspace: string }) => ({ workspace: x.workspace })), { workspace: workspace.trim(), token: token.trim(), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }] } })
      } else if (editIdx !== null) {
        const gi = githubIssues[editIdx]
        const updated = githubIssues.map((item: { workspace: string }, i: number) => i === editIdx
          ? { workspace: workspace.trim(), originalWorkspace: gi.workspace, ...(token.trim() ? { token: token.trim() } : {}), ...(webhookSecret.trim() ? { webhookSecret: webhookSecret.trim() } : {}) }
          : { workspace: item.workspace }
        )
        onSave({ integrations: { githubIssues: updated } })
      }
    }
    setShowModal(false)
    resetModal()
  }

  function removeTracker() {
    if (editType === "linear" && editIdx !== null) {
      onSave({ integrations: { linear: linear.filter((_: unknown, j: number) => j !== editIdx) } })
    } else if (editType === "shortcut" && editIdx !== null) {
      onSave({ integrations: { shortcut: shortcut.filter((_: unknown, j: number) => j !== editIdx).map((x: { workspace: string }) => ({ workspace: x.workspace })) } })
    } else if (editType === "github-issues" && editIdx !== null) {
      onSave({ integrations: { githubIssues: githubIssues.filter((_: unknown, j: number) => j !== editIdx).map((x: { workspace: string }) => ({ workspace: x.workspace })) } })
    }
    setShowModal(false)
    resetModal()
  }

  const trackerTypeLabel = (t: TrackerType) => {
    switch (t) {
      case "linear": return "Linear"
      case "shortcut": return "Shortcut"
      case "github-issues": return "GitHub Issues"
    }
  }

  const modalTitle = modalMode === "add"
    ? `Add ${trackerTypeLabel(modalType)} workspace`
    : `Edit ${trackerTypeLabel(editType)} — ${workspace}`

  const modalIcon = (modalMode === "add" ? modalType : editType) === "linear"
    ? <Zap className="size-4" />
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <span className="text-[#F4603C]">⚡</span>
    : <Github className="size-4" />

  const tokenHint = (modalMode === "add" ? modalType : editType) === "linear"
    ? <>Create a token at <a href="https://linear.app/settings/api" target="_blank" rel="noopener noreferrer" className="underline">linear.app/settings/api</a></>
    : (modalMode === "add" ? modalType : editType) === "github-issues"
    ? <>Create a token at <a href="https://github.com/settings/tokens" target="_blank" rel="noopener noreferrer" className="underline">github.com/settings/tokens</a> with <code>repo</code> and <code>issues</code> scopes</>
    : null

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Issue Trackers</h2>
        <p className="text-sm text-muted-foreground mb-4">Connect issue trackers to sync and create issues from Factories.</p>

        {/* Summary badges */}
        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {allTrackers.length} workspace{allTrackers.length !== 1 ? "s" : ""} connected
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

      {/* Configured trackers list */}
      {allTrackers.length > 0 && (
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

      {allTrackers.length === 0 && (
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
        <DialogContent className="max-w-lg p-0 gap-0">
          <DialogTitle className="sr-only">{modalTitle}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <div className="flex items-center gap-2">
              {modalIcon}
              <h3 className="font-medium">{modalTitle}</h3>
            </div>
          </div>
          <div className="p-5 space-y-4">
              <p className="text-sm text-muted-foreground">
                Connect a {trackerTypeLabel(modalMode === "add" ? modalType : editType)} workspace to sync issues.
              </p>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Workspace label</label>
                <Input value={workspace} onChange={e => setWorkspace(e.target.value)} className="h-9 text-sm" placeholder="e.g. my-company" />
                <p className="text-xs text-muted-foreground mt-1">A friendly name to identify this workspace</p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                <Input type="password" value={token} onChange={e => setToken(e.target.value)} className="h-9 text-sm" placeholder={`${trackerTypeLabel(modalMode === "add" ? modalType : editType)} API token`} />
                {tokenHint && <p className="text-xs text-muted-foreground mt-1">{tokenHint}</p>}
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
                <Input type="password" value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)} className="h-9 text-sm" placeholder="Webhook secret for signature verification" />
                <p className="text-xs text-muted-foreground mt-1">Used to verify incoming webhook signatures. Leave blank to keep existing.</p>
              </div>
            </div>
            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={removeTracker}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetModal() }}>Cancel</Button>
                <Button size="sm" disabled={saving || !workspace.trim() || (modalMode === "add" && !token.trim())} onClick={saveTracker}>
                  {modalMode === "add" ? "Add workspace" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}



function WebhooksSection({ hubUrl }: { hubUrl: string }) {
  const [copied, setCopied] = useState<string | null>(null)

  const urls = [
    {
      name: "Linear",
      url: hubUrl ? `${hubUrl}/api/integrations/linear/webhook` : "",
      hint: "Paste into Linear → Settings → API → Webhooks, subscribe to Issue events.",
    },
    {
      name: "Shortcut",
      url: hubUrl ? `${hubUrl}/api/integrations/shortcut/webhook` : "",
      hint: "Use Shortcut's API to register this webhook: POST /api/v3/webhooks with this URL.",
    },
    {
      name: "GitHub Issues",
      url: hubUrl ? `${hubUrl}/api/integrations/github-issues/webhook` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Issues events.",
    },
    {
      name: "GitHub (PRs)",
      url: hubUrl ? `${hubUrl}/api/integrations/github/webhook` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Pull requests and Issue comments.",
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
        <h2 className="text-base font-semibold mb-1">Webhook URLs</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Use these URLs to configure webhooks in your integrations.
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

function FactoriesSection({ hubUrl, settings, onSave, onSaveSilent, saving }: { hubUrl: string; settings: SettingsData; onSave: (p: object) => Promise<boolean>; onSaveSilent: (p: object) => void; saving: boolean }) {
  const [savedFactory, setSavedFactory] = useState<string | null>(null)
  const factories = settings.factories || []
  const token = typeof window !== "undefined" ? (sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || "") : ""

  // Concurrency groups state — use local editable state so inputs are
  // responsive, then debounce saves to the server.
  const groups = settings.concurrencyGroups || [{ name: "global", limit: 0 }]
  const groupsRef = useRef(groups)
  groupsRef.current = groups

  const [groupLimits, setGroupLimits] = useState<Record<string, number>>(() => {
    const init: Record<string, number> = {}
    for (const g of groups) init[g.name] = g.limit
    return init
  })
  // Keep local state in sync when props change from outside (e.g. initial load)
  useEffect(() => {
    const next: Record<string, number> = {}
    for (const g of groups) next[g.name] = g.limit
    setGroupLimits(next)
  }, [settings.concurrencyGroups])

  const [newGroupName, setNewGroupName] = useState("")
  const [newGroupLimit, setNewGroupLimit] = useState(0)

  const allGroupNames = groups.map(g => g.name)

  function saveGroups(updatedGroups: typeof groups) {
    onSave({ concurrencyGroups: updatedGroups.map(g => ({ name: g.name, limit: g.limit })) })
  }

  function addGroup() {
    if (!newGroupName.trim() || groups.some(g => g.name === newGroupName.trim())) return
    const updated = [...groups, { name: newGroupName.trim(), limit: newGroupLimit }]
    setNewGroupName("")
    setNewGroupLimit(0)
    saveGroups(updated)
  }

  function removeGroup(name: string) {
    if (name === "global") return
    saveGroups(groups.filter(g => g.name !== name))
  }

  // Debounce group limit updates to avoid firing a PATCH on every keystroke.
  // Each group gets its own timer so edits to different groups don't cancel
  // each other — without this, editing group A then group B within 500ms would
  // silently drop group A's change.
  const groupLimitTimersRef = useRef<Record<string, ReturnType<typeof setTimeout>>>({})

  function updateGroupLimit(name: string, limit: number) {
    setGroupLimits(prev => ({ ...prev, [name]: limit }))
    const timers = groupLimitTimersRef.current
    if (timers[name]) {
      clearTimeout(timers[name])
    }
    timers[name] = setTimeout(() => {
      delete timers[name]
      const latest = groupsRef.current
      const updated = latest.map(g => g.name === name ? { ...g, limit } : g)
      saveGroups(updated)
    }, 500)
  }

  function updateFactoryGroup(factoryIdx: number, groupName: string) {
    const updated = factories.map((x, j) => j === factoryIdx ? { ...x, concurrencyGroup: groupName } : x)
    setSavedFactory(factories[factoryIdx].name)
    setTimeout(() => setSavedFactory(null), 1500)
    onSaveSilent({ factories: updated })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Factories</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Automatically spawn and terminate claws based on issue tracker events.
        </p>
      </div>

      {/* Concurrency Groups */}
      <div className="rounded-lg border border-border bg-muted/30 p-4 space-y-4">
        <div>
          <p className="font-medium text-sm">Concurrency Groups</p>
          <p className="text-muted-foreground text-xs mt-0.5">
            Limit how many claws can run simultaneously per group. 0 = unlimited.
          </p>
        </div>

        {/* Groups table */}
        <div className="space-y-2">
          {groups.map(g => (
            <div key={g.name} className="flex items-center gap-3">
              <span className="text-sm font-mono w-24 shrink-0">{g.name}</span>
              <Input
                type="number"
                min={0}
                value={groupLimits[g.name] ?? g.limit}
                onChange={(e) => updateGroupLimit(g.name, parseInt(e.target.value) || 0)}
                className="w-24 text-sm h-7"
                placeholder="0 = unlimited"
              />
              <span className="text-xs text-muted-foreground">limit</span>
              {g.name !== "global" && (
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive hover:text-destructive h-7 px-2"
                  onClick={() => removeGroup(g.name)}
                >
                  <Trash2 className="size-3.5" />
                </Button>
              )}
            </div>
          ))}
        </div>

        {/* Add group */}
        <div className="flex items-center gap-2 pt-2 border-t border-border">
          <Input
            placeholder="Group name"
            value={newGroupName}
            onChange={(e) => setNewGroupName(e.target.value)}
            className="w-32 text-sm h-7"
            onKeyDown={(e) => { if (e.key === "Enter") addGroup() }}
          />
          <Input
            type="number"
            min={0}
            value={newGroupLimit}
            onChange={(e) => setNewGroupLimit(parseInt(e.target.value) || 0)}
            className="w-24 text-sm h-7"
            placeholder="Limit"
          />
          <Button size="sm" variant="outline" className="h-7" onClick={addGroup} disabled={!newGroupName.trim()}>
            + Add Group
          </Button>
        </div>
      </div>

      {factories.length > 0 && (
        <div className="space-y-2">
          {factories.map((f, i) => (
            <div key={i} className={cn("border rounded-lg p-4 flex items-center justify-between", (f.enabled ?? true) ? "border-border" : "border-border/50 opacity-60")}>
              <div>
                <div className="flex items-center gap-2">
                  <p className="text-sm font-medium">{f.name}</p>
                  {!(f.enabled ?? true) && <span className="text-xs text-muted-foreground bg-muted px-1.5 py-0.5 rounded">paused</span>}
                </div>
                <p className="text-xs text-muted-foreground">
                  {f.integration} · {f.workspace} · &ldquo;{f.triggerStatus}&rdquo; → {f.template}
                  {f.labels && f.labels.length > 0 && ` · labels: ${f.labels.join(", ")}`}
                  {f.assigned_to && ` · assigned: ${f.assigned_to}`}
                  {" · webhook: "}{f.webhookSecretSet ? <span className="text-green-500">✓</span> : <span className="text-amber-500">not set</span>}
                </p>
              </div>
              <div className="flex items-center gap-2">
                {savedFactory === f.name && (
                  <span className="text-xs text-green-500">✓</span>
                )}
                {/* Concurrency group dropdown */}
                <select
                  value={f.concurrencyGroup || "global"}
                  onChange={(e) => updateFactoryGroup(i, e.target.value)}
                  className="text-xs rounded-md border border-border bg-background px-2 py-1 h-7"
                >
                  {allGroupNames.map(name => (
                    <option key={name} value={name}>{name}</option>
                  ))}
                </select>
                <button
                  onClick={async () => {
                    const enabled = !(f.enabled ?? true)
                    const updated = factories.map((x, j) => j === i ? { ...x, enabled } : x)
                    setSavedFactory(f.name)
                    setTimeout(() => setSavedFactory(null), 1500)
                    onSaveSilent({ factories: updated })
                  }}
                  className={cn(
                    "relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full transition-colors duration-200",
                    (f.enabled ?? true)
                      ? "bg-green-600 border-2 border-transparent"
                      : "bg-transparent border-2 border-muted-foreground/40"
                  )}
                  title={(f.enabled ?? true) ? "Pause factory" : "Enable factory"}
                >
                  <span className={cn(
                    "pointer-events-none inline-block size-4 rounded-full shadow-sm transform transition-transform duration-200",
                    (f.enabled ?? true)
                      ? "bg-white translate-x-4"
                      : "bg-muted-foreground/50 translate-x-0"
                  )} />
                </button>
                <Button size="sm" variant="outline" onClick={() => window.open(`/factories?name=${encodeURIComponent(f.name)}`, '_self')}>Activity</Button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}



function SecretsSection({ settings }: { settings: SettingsData | null }) {
  const [secrets, setSecrets] = useState<string[]>([])
  const [newName, setNewName] = useState("")
  const [newValue, setNewValue] = useState("")
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const hubUrl = getHubUrl()
  const token = () => sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""

  const refresh = useCallback(async () => {
    try {
      const res = await fetch(`${hubUrl}/api/secrets`, { headers: { Authorization: `Bearer ${token()}` } })
      if (res.ok) {
        const data = await res.json()
        setSecrets(data.secrets || [])
      }
    } finally {
      setLoading(false)
    }
  }, [hubUrl])

  // Load secrets from API on mount — the authoritative source of truth.
  // The /api/secrets endpoint reads from disk, so manually-edited hub.yaml
  // entries are visible immediately without waiting for a server restart.
  useEffect(() => {
    refresh()
  }, [refresh])

  const handleAdd = async () => {
    if (!newName.trim() || !newValue.trim()) return
    setSaving(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}/api/secrets`, {
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
    const res = await fetch(`${hubUrl}/api/secrets?name=${encodeURIComponent(name)}`, {
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
          Named secrets referenced by factories via <code className="bg-muted px-1 rounded text-xs">webhook_secret_ref</code>.
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
  const token = () => sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""

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
          Model Context Protocol servers add tools to your claws. Configure them here and reference them in templates.
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

function TemplatesSection() {
  const [templates, setTemplates] = useState<{ name: string; updatedAt: string }[]>([])
  const [loading, setLoading] = useState(true)
  const [deleting, setDeleting] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const hubUrl = getHubUrl()
      const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
      const res = await fetch(`${hubUrl}/api/templates`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (res.ok) setTemplates(await res.json())
    } catch {}
    setLoading(false)
  }, [])

  useEffect(() => { load() }, [load])

  const handleDelete = async (name: string) => {
    setDeleting(name)
    try {
      const hubUrl = getHubUrl()
      const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
      const res = await fetch(`${hubUrl}/api/templates/${encodeURIComponent(name)}`, {
        method: "DELETE",
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!res.ok) throw new Error(await res.text())
      await load()
    } catch {}
    setDeleting(null)
    setConfirmDelete(null)
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Templates</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Templates pushed to the hub and available for claw creation.
          Push new templates with <code className="bg-muted px-1 rounded text-xs">elasticclaw template push</code>.
        </p>
      </div>

      {loading ? (
        <p className="text-sm text-muted-foreground animate-pulse">Loading…</p>
      ) : templates.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No templates pushed yet. Use <code className="bg-muted px-1 rounded text-xs">elasticclaw template push &lt;name&gt;</code> to add one.
        </p>
      ) : (
        <div className="space-y-2">
          {templates.map((t) => (
            <div key={t.name} className="border border-border rounded-lg p-4 flex items-center justify-between">
              <div>
                <p className="text-sm font-mono font-medium">{t.name}</p>
                <p className="text-xs text-muted-foreground">
                  Updated {new Date(t.updatedAt).toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" })}
                </p>
              </div>
              {confirmDelete === t.name ? (
                <div className="flex items-center gap-2">
                  <span className="text-xs text-muted-foreground">Delete {t.name}?</span>
                  <Button size="sm" variant="destructive" disabled={deleting === t.name}
                    onClick={() => handleDelete(t.name)}>
                    {deleting === t.name ? "Deleting…" : "Delete"}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => setConfirmDelete(null)}>Cancel</Button>
                </div>
              ) : (
                <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-destructive"
                  onClick={() => setConfirmDelete(t.name)}>
                  <Trash2 className="size-3.5" />
                </Button>
              )}
            </div>
          ))}
        </div>
      )}
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
  const token = () => sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""

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
                placeholder="e.g. Factory webhook is firing but the machine isn't being bootstrapped..."
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
      const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
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
          <Button size="sm" variant="outline" onClick={() => load(true)} disabled={loading}>
            <RotateCcw className={cn("size-3.5 mr-1.5", loading && "animate-spin")} />
            {loading ? "Checking…" : "Refresh"}
          </Button>
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

// ─── Analytics Section ───────────────────────────────────────────────────────

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
  byTriggerStatus: Record<string, number>
  recentEvents: Array<{
    id: string
    factoryName: string
    issueId: string
    clawId: string
    action: string
    detail: string
    result: string
    createdAt: string
  }>
  computeError?: string
}

function AnalyticsSection() {
  const [summaries, setSummaries] = useState<AnalyticsSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [days, setDays] = useState(30)
  const [selectedFactory, setSelectedFactory] = useState<string | null>(null)
  const [partialData, setPartialData] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    setPartialData(false)
    try {
      const hubUrl = getHubUrl()
      const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
      const res = await fetch(`${hubUrl}/api/analytics/factories?days=${days}`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!res.ok) throw new Error(await res.text())
      setPartialData(res.headers.get("X-Analytics-Partial") === "true")
      const data = await res.json()
      setSummaries(data || [])
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load analytics")
    } finally {
      setLoading(false)
    }
  }, [days])

  useEffect(() => { load() }, [load])

  const totalTriggers = summaries.reduce((sum, s) => sum + s.totalTriggers, 0)
  const totalSuccess = summaries.reduce((sum, s) => sum + s.successfulCreations, 0)
  const totalFailed = summaries.reduce((sum, s) => sum + s.failedCreations, 0)
  const totalPRsOpened = summaries.reduce((sum, s) => sum + s.prOpened, 0)
  const totalPRsMerged = summaries.reduce((sum, s) => sum + s.prMerged, 0)
  const overallSuccessRate = totalTriggers > 0 ? (totalSuccess / totalTriggers * 100).toFixed(1) : "0"
  const overallMergeRate = totalPRsOpened > 0 ? (totalPRsMerged / totalPRsOpened * 100).toFixed(1) : "0"

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Factory Analytics</h2>
        <p className="text-sm text-muted-foreground mb-4">
          Usage and success metrics for factories. Data is retained for up to 1 year.
        </p>
      </div>

      <div className="flex items-center gap-4">
        <label className="text-sm text-muted-foreground">Time range:</label>
        <select
          value={days}
          onChange={e => setDays(Number(e.target.value))}
          className="h-8 rounded-md border border-border bg-background px-2 text-sm"
        >
          <option value={7}>Last 7 days</option>
          <option value={30}>Last 30 days</option>
          <option value={90}>Last 90 days</option>
        </select>
        <Button size="sm" variant="outline" onClick={load} disabled={loading}>
          <RotateCcw className="size-3 mr-1" /> Refresh
        </Button>
      </div>

      {error && <p className="text-sm text-red-500">{error}</p>}

      {partialData && (
        <div className="flex items-center gap-2 text-sm text-yellow-500 bg-yellow-500/10 border border-yellow-500/20 rounded-lg p-3">
          <AlertTriangle className="size-4 shrink-0" />
          Some factory data could not be loaded. Metrics shown may be incomplete.
        </div>
      )}

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading analytics...</p>
      ) : summaries.length === 0 ? (
        <div className="border border-dashed border-border rounded-lg p-8 text-center">
          <BarChart3 className="size-8 text-muted-foreground mx-auto mb-3" />
          <p className="text-sm text-muted-foreground">No analytics data yet</p>
          <p className="text-xs text-muted-foreground mt-1">
            Factory activity will appear here once factories start creating claws.
          </p>
        </div>
      ) : (
        <>
          {/* Overall stats */}
          <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
            <div className="border border-border rounded-lg p-4">
              <p className="text-xs text-muted-foreground uppercase">Total Triggers</p>
              <p className="text-2xl font-semibold mt-1">{totalTriggers}</p>
            </div>
            <div className="border border-border rounded-lg p-4">
              <p className="text-xs text-muted-foreground uppercase">Success Rate</p>
              <p className="text-2xl font-semibold mt-1">{overallSuccessRate}%</p>
            </div>
            <div className="border border-border rounded-lg p-4">
              <p className="text-xs text-muted-foreground uppercase">PRs Opened</p>
              <p className="text-2xl font-semibold mt-1">{totalPRsOpened}</p>
            </div>
            <div className="border border-border rounded-lg p-4">
              <p className="text-xs text-muted-foreground uppercase">PR Merge Rate</p>
              <p className="text-2xl font-semibold mt-1">{overallMergeRate}%</p>
            </div>
          </div>

          {/* Per-factory breakdown */}
          <div className="space-y-4">
            <h3 className="text-sm font-medium">Per Factory</h3>
            {summaries.map(summary => (
              <div key={summary.factoryName} className="border border-border rounded-lg overflow-hidden">
                <button
                  onClick={() => setSelectedFactory(selectedFactory === summary.factoryName ? null : summary.factoryName)}
                  className="w-full flex items-center justify-between p-4 hover:bg-muted/50 transition-colors text-left"
                >
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <p className="font-medium">{summary.factoryName}</p>
                      {summary.computeError && (
                        <span className="inline-flex items-center gap-1 text-xs bg-red-500/20 text-red-400 px-1.5 py-0.5 rounded">
                          <AlertTriangle className="size-3" /> Error
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      {summary.totalTriggers} triggers · {summary.successRate.toFixed(1)}% success · {summary.prOpened} PRs · {summary.prMergeRate.toFixed(1)}% merge rate
                    </p>
                    {summary.computeError && (
                      <p className="text-xs text-red-400 mt-1">{summary.computeError}</p>
                    )}
                  </div>
                  <ArrowRight className={cn("size-4 transition-transform", selectedFactory === summary.factoryName ? "rotate-90" : "")} />
                </button>

                {selectedFactory === summary.factoryName && (
                  <div className="border-t border-border p-4 space-y-4">
                    <div className="grid grid-cols-3 gap-4">
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">Created</p>
                        <p className="text-lg font-semibold">{summary.successfulCreations}</p>
                      </div>
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">Failed</p>
                        <p className="text-lg font-semibold text-red-500">{summary.failedCreations}</p>
                      </div>
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">Terminated</p>
                        <p className="text-lg font-semibold">{summary.terminations}</p>
                      </div>
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">PRs Opened</p>
                        <p className="text-lg font-semibold">{summary.prOpened}</p>
                      </div>
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">PRs Merged</p>
                        <p className="text-lg font-semibold text-green-500">{summary.prMerged}</p>
                      </div>
                      <div className="bg-muted rounded-lg p-3">
                        <p className="text-xs text-muted-foreground">PRs Closed</p>
                        <p className="text-lg font-semibold text-orange-500">{summary.prClosed}</p>
                      </div>
                    </div>

                    {Object.keys(summary.byTriggerStatus).length > 0 && (
                      <div>
                        <p className="text-xs font-medium mb-2">By Trigger Status</p>
                        <div className="flex flex-wrap gap-2">
                          {Object.entries(summary.byTriggerStatus).map(([status, count]) => (
                            <span key={status} className="text-xs bg-muted px-2 py-1 rounded">
                              {status}: {count}
                            </span>
                          ))}
                        </div>
                      </div>
                    )}

                    {summary.recentEvents.length > 0 && (
                      <div>
                        <p className="text-xs font-medium mb-2">Recent Events</p>
                        <div className="space-y-1 max-h-48 overflow-y-auto">
                          {summary.recentEvents.slice(0, 20).map(event => (
                            <div key={event.id} className="flex items-center gap-2 text-xs p-2 rounded hover:bg-muted/50">
                              <span className={cn(
                                "px-1.5 py-0.5 rounded font-medium",
                                event.action === "claw_created" && "bg-green-500/20 text-green-400",
                                event.action === "error" && "bg-red-500/20 text-red-400",
                                event.action === "claw_terminated" && "bg-orange-500/20 text-orange-400",
                                event.action === "pr_opened" && "bg-blue-500/20 text-blue-400",
                                event.action === "pr_merged" && "bg-purple-500/20 text-purple-400",
                                event.action === "pr_closed" && "bg-red-500/20 text-red-400",
                                event.action === "done_signal" && "bg-emerald-500/20 text-emerald-400",
                              )}>
                                {event.action}
                              </span>
                              <span className="text-muted-foreground">{event.issueId}</span>
                              {event.detail && <span className="text-muted-foreground truncate">{event.detail}</span>}
                              <span className="ml-auto text-muted-foreground/50">
                                {new Date(event.createdAt).toLocaleDateString()}
                              </span>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                )}
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  )
}
