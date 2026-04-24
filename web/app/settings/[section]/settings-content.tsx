"use client"

import { useParams, useRouter } from "next/navigation"
import { useEffect, useState, useCallback } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Factory, Copy, Check, LayoutTemplate, Trash2, Lock, Sparkles, Send, RotateCcw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"
import Link from "next/link"

type Section = "runtimes" | "llm" | "github" | "security" | "integrations" | "factories" | "secrets" | "templates" | "ai-config"

const VALID_SECTIONS: Section[] = ["runtimes", "llm", "github", "security", "integrations", "factories", "secrets", "templates", "ai-config"]

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
}

async function fetchSettings(): Promise<SettingsData> {
  const hubUrl = getHubUrl()
  const token = sessionStorage.getItem("ec_hub_token") || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error("Failed to load settings")
  return res.json()
}

async function patchSettings(patch: object): Promise<void> {
  const hubUrl = getHubUrl()
  const token = sessionStorage.getItem("ec_hub_token") || ""
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
    const token = sessionStorage.getItem("ec_hub_token") || ""
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
    { id: "security", label: "Security", icon: Shield },
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
        <main className="flex-1 overflow-y-auto p-8 max-w-2xl">
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
          {settings && section === "security" && (
            <SecuritySection onSave={save} saving={saving} />
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

function SecuritySection({ onSave, saving }: { onSave: (p: object) => void; saving: boolean }) {
  const [newPw, setNewPw] = useState("")
  const [confirm, setConfirm] = useState("")
  const [pwErr, setPwErr] = useState("")

  function handlePasswordSave() {
    setPwErr("")
    if (newPw.length < 8) { setPwErr("Password must be at least 8 characters"); return }
    if (newPw !== confirm) { setPwErr("Passwords don\'t match"); return }
    onSave({ uiPassword: newPw })
    setNewPw(""); setConfirm("")
  }

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">Security</h2>
      <p className="text-sm text-muted-foreground mb-4">Change the web UI login password.</p>
      <div className="border border-border rounded-lg p-5 max-w-sm space-y-3">
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">New Password</label>
          <Input type="password" value={newPw} onChange={e => setNewPw(e.target.value)} className="h-8 text-sm" placeholder="Min 8 characters" />
        </div>
        <div>
          <label className="text-xs text-muted-foreground mb-1 block">Confirm Password</label>
          <Input type="password" value={confirm} onChange={e => setConfirm(e.target.value)} className="h-8 text-sm" placeholder="Repeat password" />
        </div>
        {pwErr && <p className="text-xs text-red-500">{pwErr}</p>}
        <Button size="sm" disabled={saving || !newPw || !confirm} onClick={handlePasswordSave}>
          Change Password
        </Button>
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
  const token = () => sessionStorage.getItem("ec_hub_token") || ""

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
}

function AIConfigSection() {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState("")
  const [loading, setLoading] = useState(false)
  const [proposedYaml, setProposedYaml] = useState<string | null>(null)
  const [placeholders, setPlaceholders] = useState<string[]>([])
  const [secretValues, setSecretValues] = useState<Record<string, string>>({})
  const [backupPath, setBackupPath] = useState<string | null>(null)
  const [applying, setApplying] = useState(false)
  const [reverting, setReverting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [applySuccess, setApplySuccess] = useState(false)

  const hubUrl = getHubUrl()
  const token = () => sessionStorage.getItem("ec_hub_token") || ""

  // Check for existing backup on mount
  useEffect(() => {
    fetch(`${hubUrl}/api/settings/ai-config/backup`, {
      headers: { Authorization: `Bearer ${token()}` },
    })
      .then(r => r.json())
      .then(d => { if (d.backup_path) setBackupPath(d.backup_path) })
      .catch(() => {})
  }, [])

  const sendMessage = async () => {
    if (!input.trim() || loading) return
    const userMsg: ChatMessage = { role: "user", content: input.trim() }
    const newMessages = [...messages, userMsg]
    setMessages(newMessages)
    setInput("")
    setLoading(true)
    setError(null)

    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({
          message: userMsg.content,
          history: messages,
        }),
      })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setMessages(prev => [...prev, { role: "assistant", content: data.reply }])
      if (data.proposed_yaml) {
        setProposedYaml(data.proposed_yaml)
        setPlaceholders(data.placeholders || [])
        setSecretValues({})
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed")
    } finally {
      setLoading(false)
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
    } catch (e) {
      setError(e instanceof Error ? e.message : "Revert failed")
    } finally {
      setReverting(false)
    }
  }

  const allPlaceholdersFilled = placeholders.every(p => secretValues[p]?.trim())

  return (
    <div className="space-y-4 -mx-8 px-0" style={{ maxWidth: "none", width: "calc(100% + 4rem)" }}>
      <div className="px-8">
        <h2 className="text-base font-semibold mb-1">Configure with AI</h2>
        <p className="text-sm text-muted-foreground">
          Describe changes in plain English. The AI will propose a hub.yaml update for you to review and apply.
        </p>
      </div>

      {error && (
        <div className="px-8">
          <p className="text-sm text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>
        </div>
      )}

      {applySuccess && !backupPath && (
        <div className="px-8">
          <p className="text-sm text-green-600 bg-green-50 dark:bg-green-950/30 px-3 py-2 rounded-lg">Config applied successfully.</p>
        </div>
      )}

      {applySuccess && backupPath && (
        <div className="px-8 flex items-center gap-3">
          <p className="text-sm text-green-600">✓ Config applied.</p>
          <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
            <RotateCcw className="size-3.5 mr-1" />
            {reverting ? "Reverting…" : "Revert"}
          </Button>
        </div>
      )}

      {backupPath && !applySuccess && (
        <div className="px-8 flex items-center gap-3">
          <p className="text-xs text-muted-foreground">Previous backup available.</p>
          <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
            <RotateCcw className="size-3.5 mr-1" />
            {reverting ? "Reverting…" : "Revert to backup"}
          </Button>
        </div>
      )}

      <div className="flex gap-4 px-8" style={{ alignItems: "flex-start" }}>
        {/* Left: Chat */}
        <div className="flex flex-col min-w-0" style={{ flex: "1 1 0" }}>
          {/* Message history */}
          <div className="border border-border rounded-lg bg-muted/20 flex flex-col" style={{ minHeight: 320, maxHeight: 480 }}>
            <div className="flex-1 overflow-y-auto p-4 space-y-3">
              {messages.length === 0 && (
                <p className="text-sm text-muted-foreground text-center py-8">
                  Describe the config change you&apos;d like to make.
                </p>
              )}
              {messages.map((m, i) => (
                <div key={i} className={cn("flex", m.role === "user" ? "justify-end" : "justify-start")}>
                  <div
                    className={cn(
                      "max-w-[85%] rounded-xl px-3 py-2 text-sm whitespace-pre-wrap break-words",
                      m.role === "user"
                        ? "bg-primary text-primary-foreground"
                        : "bg-muted text-foreground"
                    )}
                  >
                    {m.content}
                  </div>
                </div>
              ))}
              {loading && (
                <div className="flex justify-start">
                  <div className="bg-muted rounded-xl px-3 py-2 text-sm text-muted-foreground animate-pulse">Thinking…</div>
                </div>
              )}
            </div>

            {/* Input */}
            <div className="border-t border-border p-3 flex gap-2">
              <Input
                placeholder="e.g. Add a Linear integration for workspace acme"
                value={input}
                onChange={e => setInput(e.target.value)}
                onKeyDown={e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendMessage() } }}
                disabled={loading}
                className="flex-1 text-sm"
              />
              <Button size="icon" onClick={sendMessage} disabled={loading || !input.trim()}>
                <Send className="size-4" />
              </Button>
            </div>
          </div>
        </div>

        {/* Right: Proposed config panel */}
        {proposedYaml && (
          <div className="flex flex-col gap-4 min-w-0" style={{ flex: "1 1 0" }}>
            <div className="border border-border rounded-lg">
              <div className="px-4 py-2 border-b border-border bg-muted/40">
                <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Proposed hub.yaml</p>
              </div>
              <pre className="p-4 text-xs font-mono overflow-auto bg-muted/20 rounded-b-lg" style={{ maxHeight: 300 }}>
                {proposedYaml}
              </pre>
            </div>

            {placeholders.length > 0 && (
              <div className="border border-border rounded-lg p-4 space-y-3">
                <p className="text-sm font-medium">Fill in secrets</p>
                {placeholders.map(ph => (
                  <div key={ph}>
                    <label className="text-xs text-muted-foreground font-mono mb-1 block">{ph}</label>
                    <Input
                      type="password"
                      placeholder={`Value for ${ph}`}
                      value={secretValues[ph] || ""}
                      onChange={e => setSecretValues(prev => ({ ...prev, [ph]: e.target.value }))}
                      className="font-mono text-sm"
                    />
                  </div>
                ))}
              </div>
            )}

            <Button
              onClick={applyConfig}
              disabled={applying || !allPlaceholdersFilled}
              className="w-full"
            >
              {applying ? "Applying…" : "Apply Configuration"}
            </Button>
          </div>
        )}
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
      const token = sessionStorage.getItem("ec_hub_token") || ""
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
      const token = sessionStorage.getItem("ec_hub_token") || ""
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
