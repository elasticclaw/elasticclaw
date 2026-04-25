"use client"

import { useParams, useRouter } from "next/navigation"
import React, { useEffect, useState, useCallback, useRef } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Factory, Copy, Check, LayoutTemplate, Trash2, Lock, Sparkles, Send, RotateCcw, Eye, EyeOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"
import Link from "next/link"

type Section = "runtimes" | "llm" | "github" | "authentication" | "integrations" | "factories" | "secrets" | "templates" | "ai-config"

const VALID_SECTIONS: Section[] = ["runtimes", "llm", "github", "authentication", "integrations", "factories", "secrets", "templates", "ai-config"]

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
  }>
  github: Array<{ appId: number; url?: string; keySet: boolean }>
  sshPublicKeys: string[]
  integrations?: {
    linear?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    shortcut?: Array<{ workspace: string; tokenSet: boolean }>
  }
  factories?: Array<{
    name: string; integration: string; workspace: string; team: string
    triggerStatus: string; doneStatus: string; terminateOnLeave: boolean; template: string
    webhookSecretSet?: boolean; tags?: string[]; color?: string; enabled?: boolean; labels?: string[]; assigned_to?: string
  }>
  secrets?: string[]
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

  const navItems: { id: Section; label: string; icon: React.ElementType }[] = [
    { id: "runtimes", label: "Sandbox Runtimes", icon: Cpu },
    { id: "llm", label: "LLM Keys", icon: Key },
    { id: "github", label: "GitHub Apps", icon: Github },
    { id: "authentication", label: "Authentication", icon: Shield },
    { id: "integrations", label: "Integrations", icon: Zap },
    { id: "factories", label: "Factories", icon: Factory },
    { id: "secrets", label: "Secrets", icon: Lock },
    { id: "templates", label: "Templates", icon: LayoutTemplate },
    { id: "ai-config", label: "Configure with AI", icon: Sparkles },
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
          {navItems.map(({ id, label, icon: Icon }) => (
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
          {version && (
            <p className="text-xs text-muted-foreground/50 px-3 pt-4 font-mono">
              v{version}
            </p>
          )}
        </aside>

        {/* Content */}
        <main className={section === "ai-config" ? "flex-1 min-h-0 flex flex-col overflow-hidden" : "flex-1 overflow-y-auto p-8 max-w-2xl"}>
          {error && <p className="mb-4 text-sm text-red-500">{error}</p>}
          {success && <p className="mb-4 text-sm text-green-500">{success}</p>}

          {settings && section === "runtimes" && (
            <RuntimesSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "llm" && (
            <LLMSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "github" && (
            <GitHubSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "authentication" && (
            <AuthenticationSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "integrations" && (
            <IntegrationsSection settings={settings} onSave={save} saving={saving} />
          )}
          {settings && section === "factories" && (
            <FactoriesSection hubUrl={hubPublicUrl} settings={settings} onSave={save} onSaveSilent={saveSilent} saving={saving} />
          )}
          {section === "secrets" && (
            <SecretsSection settings={settings} />
          )}
          {section === "templates" && (
            <TemplatesSection />
          )}
          {section === "ai-config" && (
            <AIConfigSection />
          )}
        </main>
      </div>
    </div>
  )
}

function RuntimesSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const [newKey, setNewKey] = useState("")
  const rep = settings.providers?.replicated
  const day = settings.providers?.daytona

  const [repToken, setRepToken] = useState("")
  const [repTTL, setRepTTL] = useState(rep?.defaultTtl || "48h")
  const [repType, setRepType] = useState(rep?.defaultInstanceType || "r1.large")

  const [dayURL, setDayURL] = useState(day?.apiUrl || "")
  const [dayKey, setDayKey] = useState("")
  const [daySnapshot, setDaySnapshot] = useState(day?.defaultSnapshot || "")

  return (
    <div className="space-y-8">
      <div>
        <h2 className="text-base font-semibold mb-1">Sandbox Runtimes</h2>
        <p className="text-sm text-muted-foreground mb-6">Configure VM providers for spawning claws.</p>

        {/* Replicated */}
        <div className="border border-border rounded-lg p-5 mb-4">
          <div className="flex items-center justify-between mb-4">
            <div>
              <h3 className="font-medium">Replicated CMX</h3>
              <p className="text-xs text-muted-foreground mt-0.5">Kubernetes-based VM provider</p>
            </div>
            {rep?.tokenSet && <span className="text-xs bg-green-500/20 text-green-400 px-2 py-1 rounded">Configured</span>}
          </div>
          <div className="space-y-3">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
              <Input
                type="password"
                placeholder={rep?.tokenSet ? "••••••••••• (set)" : "Enter Replicated API token"}
                value={repToken}
                onChange={e => setRepToken(e.target.value)}
                className="h-8 text-sm"
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default TTL</label>
                <Input value={repTTL} onChange={e => setRepTTL(e.target.value)} className="h-8 text-sm" placeholder="48h" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default Instance</label>
                <Input value={repType} onChange={e => setRepType(e.target.value)} className="h-8 text-sm" placeholder="r1.large" />
              </div>
            </div>
            <Button size="sm" disabled={saving || (!repToken && !repTTL && !repType)} onClick={() => {
              const patch: Record<string, string> = {}
              if (repTTL) patch.defaultTtl = repTTL
              if (repType) patch.defaultInstanceType = repType
              if (repToken) patch.token = repToken
              onSave({ providers: { replicated: patch } })
            }}>
              Save Replicated
            </Button>
            {/* SSH Keys for Replicated VMs */}
            <div className="border-t border-border mt-4 pt-4">
              <p className="text-xs font-medium mb-2">Additional SSH Keys</p>
              <p className="text-xs text-muted-foreground mb-3">Public keys injected into Replicated VMs at bootstrap for direct SSH access.</p>
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
          </div>
        </div>

        {/* Daytona */}
        <div className="border border-border rounded-lg p-5">
          <div className="flex items-center justify-between mb-4">
            <div>
              <h3 className="font-medium">Daytona</h3>
              <p className="text-xs text-muted-foreground mt-0.5">Development environment provider</p>
            </div>
            {day?.apiKeySet && <span className="text-xs bg-green-500/20 text-green-400 px-2 py-1 rounded">Configured</span>}
          </div>
          <div className="space-y-3">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">API URL</label>
              <Input value={dayURL} onChange={e => setDayURL(e.target.value)} className="h-8 text-sm" placeholder="https://app.daytona.io" />
            </div>
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
              <Input type="password" placeholder={day?.apiKeySet ? "••••••••••• (set)" : "Enter Daytona API key"} value={dayKey} onChange={e => setDayKey(e.target.value)} className="h-8 text-sm" />
            </div>
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Default Snapshot</label>
              <Input value={daySnapshot} onChange={e => setDaySnapshot(e.target.value)} className="h-8 text-sm" placeholder="daytona-medium" />
            </div>
            <Button size="sm" disabled={saving} onClick={() => {
              const patch: Record<string, string> = {}
              if (dayURL) patch.apiUrl = dayURL
              if (dayKey) patch.apiKey = dayKey
              if (daySnapshot) patch.defaultSnapshot = daySnapshot
              onSave({ providers: { daytona: patch } })
            }}>
              Save Daytona
            </Button>
          </div>
        </div>
      </div>
    </div>
  )
}

const PROVIDER_OPTIONS = [
  { value: "anthropic",  label: "Anthropic",  placeholder: "sk-ant-..." },
  { value: "fireworks",  label: "Fireworks",  placeholder: "fw_..." },
  { value: "other",      label: "Other",      placeholder: "" },
]

const PROVIDER_MODELS: Record<string, { id: string; name: string }[]> = {
  anthropic: [
    { id: "anthropic/claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    { id: "anthropic/claude-opus-4-5",   name: "Claude Opus 4.5" },
    { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
  ],
  fireworks: [
    { id: "fireworks/accounts/fireworks/models/kimi-k2p6",                  name: "Kimi K2" },
    { id: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct",    name: "Llama 3.3 70B" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v3",                name: "DeepSeek V3" },
  ],
}

function LLMSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const llmKeys = settings.llmKeys || []
  const [newName, setNewName] = useState("")
  const [newProvider, setNewProvider] = useState("anthropic")
  const [newCustomProvider, setNewCustomProvider] = useState("")
  const [newKey, setNewKey] = useState("")
  const [newDefault, setNewDefault] = useState(false)
  const [newDefaultModel, setNewDefaultModel] = useState("")

  const providerLabel = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.label ?? p
  const providerPlaceholder = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.placeholder ?? ""

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">LLM Keys</h2>
      <p className="text-sm text-muted-foreground mb-6">
        Named API keys injected into claws at bootstrap. The default key's model is used unless overridden by the template.
      </p>

      {/* Existing keys */}
      {llmKeys.length > 0 && (
        <div className="space-y-2 mb-6">
          {llmKeys.map((k) => (
            <div key={k.name} className="border border-border rounded-lg p-3 flex items-center justify-between gap-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">{k.name}</span>
                  <span className="text-xs text-muted-foreground">{providerLabel(k.provider)}</span>
                  {k.default && <span className="text-xs bg-primary/20 text-primary px-1.5 py-0.5 rounded">default</span>}
                  {k.keySet
                    ? <span className="text-xs text-green-500">✓ set</span>
                    : <span className="text-xs text-amber-500">✗ not set</span>}
                </div>
                {k.defaultModel && (
                  <p className="text-xs text-muted-foreground mt-0.5 truncate">model: {k.defaultModel}</p>
                )}
              </div>
              <div className="flex gap-2 shrink-0">
                {!k.default && (
                  <Button size="sm" variant="outline" disabled={saving}
                    onClick={() => onSave({ llmKeys: [{ name: k.name, default: true }] })}>
                    Set default
                  </Button>
                )}
                <Button size="sm" variant="ghost" className="text-destructive" disabled={saving}
                  onClick={() => onSave({ llmKeys: [{ name: k.name, delete: true }] })}>
                  Remove
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Add new key */}
      <div className="border border-border rounded-lg p-4 space-y-3">
        <p className="text-xs font-medium text-muted-foreground">Add key</p>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Name</label>
            <Input value={newName} onChange={e => setNewName(e.target.value)} className="h-8 text-sm" placeholder="anthropic-prod" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Provider</label>
            <select value={newProvider} onChange={e => { setNewProvider(e.target.value); setNewDefaultModel("") }}
              className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm">
              {PROVIDER_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
            </select>
          </div>
        </div>
        {newProvider === "other" && (
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Custom Provider Name</label>
            <Input value={newCustomProvider} onChange={e => setNewCustomProvider(e.target.value)}
              className="h-8 text-sm" placeholder="mistral" />
          </div>
        )}
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
          <Input type="password" value={newKey} onChange={e => setNewKey(e.target.value)}
            className="h-8 text-sm" placeholder={providerPlaceholder(newProvider)} />
        </div>
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">Default model <span className="text-muted-foreground/60">(optional)</span></label>
          {PROVIDER_MODELS[newProvider] ? (
            <select
              value={newDefaultModel}
              onChange={e => setNewDefaultModel(e.target.value)}
              className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
            >
              <option value="">— use provider default —</option>
              {PROVIDER_MODELS[newProvider].map(m => (
                <option key={m.id} value={m.id}>{m.name}</option>
              ))}
            </select>
          ) : (
            <Input value={newDefaultModel} onChange={e => setNewDefaultModel(e.target.value)}
              className="h-8 text-sm" placeholder="e.g. myprovider/model-name" />
          )}
        </div>
        <label className="flex items-center gap-2 text-sm cursor-pointer">
          <input type="checkbox" checked={newDefault} onChange={e => setNewDefault(e.target.checked)} />
          Set as default key
        </label>
        <Button size="sm" disabled={saving || !newName || !newKey || (newProvider === "other" && !newCustomProvider)}
          onClick={() => {
            const actualProvider = newProvider === "other" ? newCustomProvider : newProvider
            onSave({ llmKeys: [{ name: newName, provider: actualProvider, apiKey: newKey, default: newDefault, defaultModel: newDefaultModel || undefined }] })
            setNewName(""); setNewKey(""); setNewDefault(false); setNewCustomProvider(""); setNewDefaultModel("")
          }}>
          Add Key
        </Button>
      </div>
    </div>
  )
}

function GitHubSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const [appId, setAppId] = useState("")
  const [url, setUrl] = useState("")
  const [pem, setPem] = useState("")

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">GitHub Apps</h2>
      <p className="text-sm text-muted-foreground mb-6">
        GitHub App credentials for claws to access repositories. <span className="text-muted-foreground/60">(Optional)</span>
      </p>

      {settings.github?.length > 0 && (
        <div className="mb-6 space-y-2">
          {settings.github.map(app => (
            <div key={app.appId} className="border border-border rounded-lg p-4">
              <div className="flex items-center justify-between mb-2">
                <div>
                  <p className="text-sm font-medium">App ID: {app.appId}</p>
                  {app.url && <p className="text-xs text-muted-foreground">{app.url}</p>}
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
                    Remove
                  </Button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="border border-border rounded-lg p-5">
        <h3 className="font-medium text-sm mb-4">Add GitHub App</h3>
        <div className="space-y-3">
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
          <Button size="sm" disabled={saving || !appId || !pem || isNaN(Number(appId))} onClick={() => {
            const parsedAppId = parseInt(appId, 10)
            const newApp: { appId: number; privateKeyPem: string; url?: string } = { appId: parsedAppId, privateKeyPem: pem }
            if (url) newApp.url = url
            onSave({ github: [...(settings.github || []), newApp] })
            setAppId(""); setUrl(""); setPem("")
          }}>
            Add App
          </Button>
        </div>
      </div>
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
function IntegrationsSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const linear = settings.integrations?.linear || []
  const [editingIdx, setEditingIdx] = useState<number | null>(null)
  const [editWorkspace, setEditWorkspace] = useState("")
  const [editToken, setEditToken] = useState("")
  const [originalWorkspace, setOriginalWorkspace] = useState("")
  const [newWorkspace, setNewWorkspace] = useState("")
  const [newToken, setNewToken] = useState("")

  const startEdit = (i: number) => {
    setEditingIdx(i)
    setEditWorkspace(linear[i].workspace)
    setEditToken("")
    setOriginalWorkspace(linear[i].workspace)
  }

  const saveEdit = () => {
    if (editingIdx === null) return
    const updated = linear.map((li, i) => i === editingIdx
      ? { workspace: editWorkspace, originalWorkspace, ...(editToken ? { token: editToken } : {}) }
      : { workspace: li.workspace }
    )
    onSave({ integrations: { linear: updated } })
    setEditingIdx(null)
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Integrations</h2>
        <p className="text-sm text-muted-foreground mb-6">Connect external services to enable Factories.</p>
      </div>

      {/* Linear */}
      <div>
        <h3 className="text-sm font-semibold mb-3 flex items-center gap-2">
          <Zap className="size-4" /> Linear
        </h3>
        {linear.length > 0 && (
          <div className="mb-4 space-y-2">
            {linear.map((li, i) => (
              <div key={i}>
                {editingIdx === i ? (
                  <div className="border border-primary/40 rounded-lg p-4 space-y-3 bg-primary/5">
                    <h4 className="text-sm font-semibold">Edit: {li.workspace}</h4>
                    <p className="text-xs text-muted-foreground">Leave token/secret blank to keep existing values.</p>
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Workspace label</label>
                      <Input value={editWorkspace} onChange={e => setEditWorkspace(e.target.value)} className="h-8 text-sm" />
                    </div>
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                      <Input type="password" value={editToken} onChange={e => setEditToken(e.target.value)} className="h-8 text-sm" placeholder="lin_api_... (leave blank to keep)" />
                    </div>
                    <div className="flex gap-2">
                      <Button size="sm" disabled={saving || !editWorkspace} onClick={saveEdit}>Save</Button>
                      <Button size="sm" variant="outline" onClick={() => setEditingIdx(null)}>Cancel</Button>
                      <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive ml-auto" disabled={saving}
                        onClick={() => { onSave({ integrations: { linear: linear.filter((_, j) => j !== i) } }); setEditingIdx(null) }}>
                        Remove
                      </Button>
                    </div>
                  </div>
                ) : (
                  <div className="border border-border rounded-lg p-3 flex items-center justify-between">
                    <div>
                      <p className="text-sm font-medium">{li.workspace}</p>
                      <p className="text-xs text-muted-foreground">
                        Token: {li.tokenSet ? <span className="text-green-500">✓ set</span> : <span className="text-amber-500">✗ not set</span>}
                      </p>
                    </div>
                    <Button size="sm" variant="outline" onClick={() => startEdit(i)}>Edit</Button>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
        <div className="border border-border rounded-lg p-4 space-y-3">
          <p className="text-xs text-muted-foreground font-medium">Add a Linear workspace</p>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Workspace label</label>
            <Input value={newWorkspace} onChange={e => setNewWorkspace(e.target.value)} className="h-8 text-sm" placeholder="my-company" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
            <Input type="password" value={newToken} onChange={e => setNewToken(e.target.value)} className="h-8 text-sm" placeholder="lin_api_..." />
          </div>
          <Button size="sm" disabled={saving || !newWorkspace || !newToken}
            onClick={() => {
              onSave({ integrations: { linear: [...linear, { workspace: newWorkspace, token: newToken }] } })
              setNewWorkspace(""); setNewToken("")
            }}>
            Add Linear Workspace
          </Button>
        </div>
      </div>

      {/* Shortcut */}
      <ShortcutIntegrationsBlock settings={settings} onSave={onSave} saving={saving} />
    </div>
  )
}

function ShortcutIntegrationsBlock({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const shortcut = settings.integrations?.shortcut || []
  const [editingIdx, setEditingIdx] = useState<number | null>(null)
  const [editWorkspace, setEditWorkspace] = useState("")
  const [editToken, setEditToken] = useState("")
  const [newWorkspace, setNewWorkspace] = useState("")
  const [newToken, setNewToken] = useState("")

  return (
    <div>
      <h3 className="text-sm font-semibold mb-3 flex items-center gap-2 mt-6">
        <span className="size-4 text-[#F4603C]">⚡</span> Shortcut
      </h3>
      {shortcut.length > 0 && (
        <div className="mb-4 space-y-2">
          {shortcut.map((sc, i) => (
            <div key={i}>
              {editingIdx === i ? (
                <div className="border border-primary/40 rounded-lg p-4 space-y-3 bg-primary/5">
                  <h4 className="text-sm font-semibold">Edit: {sc.workspace}</h4>
                  <p className="text-xs text-muted-foreground">Leave token blank to keep existing.</p>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Workspace label</label>
                    <Input value={editWorkspace} onChange={e => setEditWorkspace(e.target.value)} className="h-8 text-sm" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                    <Input type="password" value={editToken} onChange={e => setEditToken(e.target.value)} className="h-8 text-sm" placeholder="(leave blank to keep)" />
                  </div>
                  <div className="flex gap-2">
                    <Button size="sm" disabled={saving || !editWorkspace} onClick={() => {
                      const patch = shortcut.map((x, j) => j === i
                        ? { workspace: editWorkspace, originalWorkspace: sc.workspace, ...(editToken ? { token: editToken } : {}) }
                        : { workspace: x.workspace }
                      )
                      onSave({ integrations: { shortcut: patch } })
                      setEditingIdx(null)
                    }}>Save</Button>
                    <Button size="sm" variant="outline" onClick={() => setEditingIdx(null)}>Cancel</Button>
                    <Button size="sm" variant="ghost" className="text-destructive ml-auto" disabled={saving}
                      onClick={() => { onSave({ integrations: { shortcut: shortcut.filter((_, j) => j !== i).map(x => ({ workspace: x.workspace })) } }); setEditingIdx(null) }}>
                      Remove
                    </Button>
                  </div>
                </div>
              ) : (
                <div className="border border-border rounded-lg p-3 flex items-center justify-between">
                  <div>
                    <p className="text-sm font-medium">{sc.workspace}</p>
                    <p className="text-xs text-muted-foreground">
                      Token: {sc.tokenSet ? <span className="text-green-500">✓ set</span> : <span className="text-amber-500">✗ not set</span>}
                    </p>
                  </div>
                  <Button size="sm" variant="outline" onClick={() => { setEditingIdx(i); setEditWorkspace(sc.workspace); setEditToken("") }}>Edit</Button>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
      <div className="border border-border rounded-lg p-4 space-y-3">
        <p className="text-xs text-muted-foreground font-medium">Add a Shortcut workspace</p>
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">Workspace label</label>
          <Input value={newWorkspace} onChange={e => setNewWorkspace(e.target.value)} className="h-8 text-sm" placeholder="my-company" />
        </div>
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
          <Input type="password" value={newToken} onChange={e => setNewToken(e.target.value)} className="h-8 text-sm" placeholder="Shortcut API token" />
        </div>
        <Button size="sm" disabled={saving || !newWorkspace || !newToken}
          onClick={() => {
            onSave({ integrations: { shortcut: [...shortcut.map(x => ({ workspace: x.workspace })), { workspace: newWorkspace, token: newToken }] } })
            setNewWorkspace(""); setNewToken("")
          }}>
          Add Shortcut Workspace
        </Button>
      </div>
    </div>
  )
}

function FactoriesSection({ hubUrl, settings, onSave, onSaveSilent, saving }: { hubUrl: string; settings: SettingsData; onSave: (p: object) => Promise<boolean>; onSaveSilent: (p: object) => void; saving: boolean }) {
  const [savedFactory, setSavedFactory] = useState<string | null>(null)
  const factories = settings.factories || []
  const [copied, setCopied] = useState(false)
  const webhookUrl = hubUrl ? `${hubUrl}/api/integrations/linear/webhook` : ""
  const handleCopy = () => {
    if (!webhookUrl) return
    navigator.clipboard.writeText(webhookUrl).then(() => { setCopied(true); setTimeout(() => setCopied(false), 2000) })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Factories</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Automatically spawn and terminate claws based on issue tracker events.
        </p>
      </div>

      {/* Webhook URLs */}
      <div className="border border-border rounded-lg p-5 space-y-4">
        <div className="space-y-2">
          <h3 className="text-sm font-medium">Linear Webhook URL</h3>
          <p className="text-xs text-muted-foreground">Paste into <strong>Linear → Settings → API → Webhooks</strong>, subscribe to <strong>Issue</strong> events.</p>
        <div className="flex items-center gap-2">
          <code className="flex-1 text-xs font-mono bg-muted px-3 py-2 rounded-md border border-border truncate">
            {webhookUrl || "Loading…"}
          </code>
          <Button variant="outline" size="sm" className="shrink-0" onClick={handleCopy} disabled={!webhookUrl}>
            {copied ? <Check className="size-3.5 text-green-500" /> : <Copy className="size-3.5" />}
            <span className="ml-1.5">{copied ? "Copied" : "Copy"}</span>
          </Button>
        </div>
        </div>
        {/* Shortcut webhook */}
        <div className="space-y-2 pt-3 border-t border-border">
          <h3 className="text-sm font-medium">Shortcut Webhook URL</h3>
          <p className="text-xs text-muted-foreground">Use Shortcut's API to register this webhook: <code className="bg-muted px-1 rounded text-xs">POST /api/v3/webhooks</code> with this URL.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 text-xs font-mono bg-muted px-3 py-2 rounded-md border border-border truncate">
              {hubUrl ? `${hubUrl}/api/integrations/shortcut/webhook` : "Loading…"}
            </code>
            <Button variant="outline" size="sm" className="shrink-0" onClick={() => {
              if (hubUrl) navigator.clipboard.writeText(`${hubUrl}/api/integrations/shortcut/webhook`)
            }} disabled={!hubUrl}>
              <Copy className="size-3.5" />
              <span className="ml-1.5">Copy</span>
            </Button>
          </div>
        </div>
        {/* GitHub webhook */}
        <div className="space-y-2 pt-3 border-t border-border">
          <h3 className="text-sm font-medium">GitHub Webhook URL</h3>
          <p className="text-xs text-muted-foreground">Use this URL when configuring webhooks in your GitHub repo or org settings. Subscribe to: <strong>Pull requests</strong> and <strong>Issue comments</strong> events.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 text-xs font-mono bg-muted px-3 py-2 rounded-md border border-border truncate">
              {hubUrl ? `${hubUrl}/api/integrations/github/webhook` : "Loading…"}
            </code>
            <Button variant="outline" size="sm" className="shrink-0" onClick={() => {
              if (hubUrl) navigator.clipboard.writeText(`${hubUrl}/api/integrations/github/webhook`)
            }} disabled={!hubUrl}>
              <Copy className="size-3.5" />
              <span className="ml-1.5">Copy</span>
            </Button>
          </div>
        </div>
      </div>

      {/* Factories-as-code callout */}
      <div className="rounded-lg border border-border bg-muted/30 p-4 text-sm">
        <p className="font-medium mb-1">Factories are managed as code</p>
        <p className="text-muted-foreground text-xs">
          Define factories in <code className="bg-muted px-1 rounded">.elasticclaw/factories/</code> and push to this hub with{" "}
          <code className="bg-muted px-1 rounded">elasticclaw factory push</code>.
          Use <code className="bg-muted px-1 rounded">elasticclaw factory create</code> to scaffold a new factory.
        </p>
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
  const [secrets, setSecrets] = useState<string[]>(settings?.secrets || [])
  const [newName, setNewName] = useState("")
  const [newValue, setNewValue] = useState("")
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const hubUrl = getHubUrl()
  const token = () => sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""

  const refresh = async () => {
    const res = await fetch(`${hubUrl}/api/secrets`, { headers: { Authorization: `Bearer ${token()}` } })
    if (res.ok) {
      const data = await res.json()
      setSecrets(data.secrets || [])
    }
  }

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
        {secrets.length === 0 ? (
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
