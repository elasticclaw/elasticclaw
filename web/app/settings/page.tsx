"use client"

import { useState, useEffect, useCallback } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Factory, Copy, Check, LayoutTemplate, Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

type Section = "runtimes" | "llm" | "github" | "security" | "integrations" | "factories" | "templates"

interface LLMKeyView {
  name: string
  provider: string
  keySet: boolean
  default: boolean
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
    webhookSecretSet?: boolean; tags?: string[]; color?: string; enabled?: boolean
  }>
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

export default function SettingsPage() {
  const [section, setSection] = useState<Section>("runtimes")
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

  async function save(patch: object) {
    setSaving(true)
    setError("")
    setSuccess("")
    try {
      await patchSettings(patch)
      setSuccess("Saved")
      await load()
      setTimeout(() => setSuccess(""), 2000)
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed")
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
    { id: "templates", label: "Templates", icon: LayoutTemplate },
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
            <button
              key={id}
              onClick={() => setSection(id)}
              className={cn(
                "w-full flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors text-left",
                section === id
                  ? "bg-primary/10 text-primary font-medium"
                  : "text-muted-foreground hover:bg-secondary hover:text-foreground"
              )}
            >
              <Icon className="size-4 flex-shrink-0" />
              {label}
            </button>
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
          {section === "templates" && (
            <TemplatesSection />
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
  { value: "fireworks",  label: "Fireworks",   placeholder: "fw-..." },
  { value: "moonshot",   label: "Moonshot (Kimi)", placeholder: "sk-..." },
  { value: "openai",     label: "OpenAI",      placeholder: "sk-..." },
  { value: "groq",       label: "Groq",        placeholder: "gsk_..." },
  { value: "deepseek",   label: "DeepSeek",    placeholder: "sk-..." },
  { value: "other",      label: "Other",       placeholder: "" },
]

function LLMSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const llmKeys = settings.llmKeys || []
  const [newName, setNewName] = useState("")
  const [newProvider, setNewProvider] = useState("anthropic")
  const [newCustomProvider, setNewCustomProvider] = useState("")
  const [newKey, setNewKey] = useState("")
  const [newDefault, setNewDefault] = useState(false)

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
            <select value={newProvider} onChange={e => setNewProvider(e.target.value)}
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
        <label className="flex items-center gap-2 text-sm cursor-pointer">
          <input type="checkbox" checked={newDefault} onChange={e => setNewDefault(e.target.checked)} />
          Set as default key
        </label>
        <Button size="sm" disabled={saving || !newName || !newKey || (newProvider === "other" && !newCustomProvider)}
          onClick={() => {
            const actualProvider = newProvider === "other" ? newCustomProvider : newProvider
            onSave({ llmKeys: [{ name: newName, provider: actualProvider, apiKey: newKey, default: newDefault }] })
            setNewName(""); setNewKey(""); setNewDefault(false); setNewCustomProvider("")
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

interface FactoryFormData {
  name: string; workspace: string; team: string
  triggerStatus: string; doneStatus: string; terminateOnLeave: boolean; template: string
  webhookSecret: string; tags: string; color: string; originalName?: string
}

function FactoriesSection({ hubUrl, settings, onSave, onSaveSilent, saving }: { hubUrl: string; settings: SettingsData; onSave: (p: object) => void; onSaveSilent: (p: object) => void; saving: boolean }) {
  const [savedFactory, setSavedFactory] = useState<string | null>(null)
  const factories = settings.factories || []
  const [copied, setCopied] = useState(false)
  const webhookUrl = hubUrl ? `${hubUrl}/api/integrations/linear/webhook` : ""
  const handleCopy = () => {
    if (!webhookUrl) return
    navigator.clipboard.writeText(webhookUrl).then(() => { setCopied(true); setTimeout(() => setCopied(false), 2000) })
  }
  const [editingFactory, setEditingFactory] = useState<number | null>(null)
  const [editForm, setEditForm] = useState<FactoryFormData>({ name: "", workspace: "", team: "", triggerStatus: "", doneStatus: "", terminateOnLeave: true, template: "", webhookSecret: "", tags: "", color: "", originalName: "" })
  const [form, setForm] = useState<FactoryFormData>({
    name: "", workspace: "", team: "", triggerStatus: "Ready for Agent",
    doneStatus: "In Review", terminateOnLeave: true, template: "base", webhookSecret: "", tags: "", color: ""
  })

  const formIntegration = form.workspace.startsWith("shortcut:") ? "shortcut" : "linear"
  const workspaces = [
    ...(settings.integrations?.linear?.map(l => ({ label: `Linear: ${l.workspace}`, value: `linear:${l.workspace}` })) || []),
    ...(settings.integrations?.shortcut?.map(s => ({ label: `Shortcut: ${s.workspace}`, value: `shortcut:${s.workspace}` })) || []),
  ]

  function update(k: keyof FactoryFormData, v: string | boolean) {
    setForm(prev => ({ ...prev, [k]: v }))
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Factories</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Automation rules that spin up claws when Linear issues enter a status.
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
      </div>

      {factories.length > 0 && (
        <div className="space-y-2 mb-6">
          {factories.map((f, i) => (
            <div key={i}>
              {editingFactory === i ? (
                <div className="border border-primary/40 rounded-lg p-4 space-y-3 bg-primary/5">
                  <h4 className="text-sm font-semibold">Edit: {f.name}</h4>
                  <div className="grid grid-cols-2 gap-3">
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                      <Input value={editForm.name} onChange={e => setEditForm(p => ({...p, name: e.target.value}))} className="h-8 text-sm" />
                    </div>
                    {f.integration !== "shortcut" && (
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Team key <span className="text-muted-foreground/60">(optional)</span></label>
                      <Input value={editForm.team} onChange={e => setEditForm(p => ({...p, team: e.target.value}))} className="h-8 text-sm" placeholder="ELA" />
                    </div>
                    )}
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Trigger status</label>
                      <Input value={editForm.triggerStatus} onChange={e => setEditForm(p => ({...p, triggerStatus: e.target.value}))} className="h-8 text-sm" />
                    </div>
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Done status</label>
                      <Input value={editForm.doneStatus} onChange={e => setEditForm(p => ({...p, doneStatus: e.target.value}))} className="h-8 text-sm" />
                    </div>
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Template</label>
                      <Input value={editForm.template} onChange={e => setEditForm(p => ({...p, template: e.target.value}))} className="h-8 text-sm" />
                    </div>
                    {f.integration !== "shortcut" && (
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Webhook Signing Secret</label>
                      <Input type="password" value={editForm.webhookSecret} onChange={e => setEditForm(p => ({...p, webhookSecret: e.target.value}))} className="h-8 text-sm" placeholder="whsec_... (leave blank to keep)" />
                    </div>
                    )}
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Tags <span className="text-muted-foreground/60">(comma-separated)</span></label>
                      <Input value={editForm.tags} onChange={e => setEditForm(p => ({...p, tags: e.target.value}))} className="h-8 text-sm" placeholder="linear, feature" />
                    </div>
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Color</label>
                      <Input value={editForm.color} onChange={e => setEditForm(p => ({...p, color: e.target.value}))} className="h-8 text-sm" placeholder="teal, coral, amber…" />
                    </div>
                  </div>
                  <label className="flex items-center gap-2 text-sm cursor-pointer">
                    <input type="checkbox" checked={editForm.terminateOnLeave} onChange={e => setEditForm(p => ({...p, terminateOnLeave: e.target.checked}))} />
                    Terminate claw when issue leaves trigger status
                  </label>
                  <div className="flex gap-2">
                    <Button size="sm" disabled={saving} onClick={() => {
                      const { webhookSecret, tags: tagsStr, color, originalName, ...rest } = editForm
                      const tags = tagsStr.split(",").map(t => t.trim()).filter(Boolean)
                      const updated = factories.map((x, j) => j === i ? { ...x, ...rest, ...(originalName ? { originalName } : {}), ...(webhookSecret ? { webhookSecret } : {}), tags, color } : x)
                      onSave({ factories: updated })
                      setEditingFactory(null)
                    }}>Save</Button>
                    <Button size="sm" variant="outline" onClick={() => setEditingFactory(null)}>Cancel</Button>
                    <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive ml-auto" disabled={saving}
                      onClick={() => { onSave({ factories: factories.filter((_, j) => j !== i) }); setEditingFactory(null) }}>
                      Remove
                    </Button>
                  </div>
                </div>
              ) : (
                <div className={cn("border rounded-lg p-4 flex items-center justify-between", (f.enabled ?? true) ? "border-border" : "border-border/50 opacity-60")}>
                  <div>
                    <div className="flex items-center gap-2">
                      <p className="text-sm font-medium">{f.name}</p>
                      {!(f.enabled ?? true) && <span className="text-xs text-muted-foreground bg-muted px-1.5 py-0.5 rounded">paused</span>}
                    </div>
                    <p className="text-xs text-muted-foreground">
                      {f.integration} · {f.team || f.workspace} · "{f.triggerStatus}" → {f.template}
                      {" · webhook: "}{f.webhookSecretSet ? <span className="text-green-500">✓</span> : <span className="text-amber-500">not set</span>}
                    </p>
                  </div>
                  <div className="flex items-center gap-2">
                    {/* Toggle switch — uses silent save to avoid global 'Saved' banner */}
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
                    <Button size="sm" variant="outline" onClick={() => {
                      setEditingFactory(i)
                      setEditForm({ name: f.name, workspace: f.workspace, team: f.team || "", triggerStatus: f.triggerStatus, doneStatus: f.doneStatus || "", terminateOnLeave: f.terminateOnLeave, template: f.template, webhookSecret: "", tags: (f.tags || []).join(", "), color: f.color || "", originalName: f.name })
                    }}>Edit</Button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <div className="border border-border rounded-lg p-5 space-y-4">
        <h3 className="text-sm font-semibold">New Factory</h3>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Factory name</label>
            <Input value={form.name} onChange={e => update("name", e.target.value)} className="h-8 text-sm" placeholder="ELA feature work" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Workspace</label>
            <select value={form.workspace} onChange={e => update("workspace", e.target.value)}
              className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm">
              <option value="">Select workspace</option>
              {workspaces.map(w => <option key={w.value} value={w.value}>{w.label}</option>)}
            </select>
          </div>
          {formIntegration === "linear" && (
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Team key <span className="text-muted-foreground/60">(optional)</span></label>
            <Input value={form.team} onChange={e => update("team", e.target.value)} className="h-8 text-sm" placeholder="ELA" />
          </div>
          )}
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Template (push to hub first)</label>
            <Input value={form.template} onChange={e => update("template", e.target.value)} className="h-8 text-sm" placeholder="base" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Trigger status</label>
            <Input value={form.triggerStatus} onChange={e => update("triggerStatus", e.target.value)} className="h-8 text-sm" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Done status</label>
            <Input value={form.doneStatus} onChange={e => update("doneStatus", e.target.value)} className="h-8 text-sm" />
          </div>
        </div>
        <label className="flex items-center gap-2 text-sm cursor-pointer">
          <input type="checkbox" checked={form.terminateOnLeave} onChange={e => update("terminateOnLeave", e.target.checked)} />
          Terminate claw when issue leaves trigger status
        </label>
        {formIntegration === "linear" && (
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Webhook Signing Secret</label>
            <Input type="password" value={form.webhookSecret} onChange={e => update("webhookSecret", e.target.value)} className="h-8 text-sm" placeholder="whsec_... from Linear webhook settings" />
          </div>
        )}
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Tags <span className="text-muted-foreground/60">(comma-separated)</span></label>
            <Input value={form.tags} onChange={e => update("tags", e.target.value)} className="h-8 text-sm" placeholder="linear, feature" />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Color</label>
            <Input value={form.color} onChange={e => update("color", e.target.value)} className="h-8 text-sm" placeholder="teal, coral, amber…" />
          </div>
        </div>
        <Button size="sm" disabled={saving || !form.name || !form.workspace || !form.template || !form.triggerStatus}
          onClick={() => {
            const { webhookSecret, tags: tagsStr, color, ...rest } = form
            const tags = tagsStr.split(",").map(t => t.trim()).filter(Boolean)
            // Determine integration type from workspace value prefix
            const integration = form.workspace.startsWith("shortcut:") ? "shortcut" : "linear"
            const workspaceName = form.workspace.includes(":") ? form.workspace.split(":")[1] : form.workspace
            onSave({ factories: [...factories, { ...rest, workspace: workspaceName, integration, ...(webhookSecret ? { webhookSecret } : {}), ...(tags.length ? { tags } : {}), ...(color ? { color } : {}) }] })
            setForm({ name: "", workspace: "", team: "", triggerStatus: "Ready for Agent", doneStatus: "In Review", terminateOnLeave: true, template: "base", webhookSecret: "", tags: "", color: "" })
          }}>
          Add Factory
        </Button>
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
