"use client"

import { useState, useEffect, useCallback } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield, Zap, Factory, Copy, Check } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

type Section = "runtimes" | "llm" | "github" | "security" | "integrations" | "factories"

interface SettingsData {
  llmKeys: Record<string, boolean>
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
  }
  factories?: Array<{
    name: string; integration: string; workspace: string; team: string
    triggerStatus: string; doneStatus: string; terminateOnLeave: boolean; template: string
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

  const navItems: { id: Section; label: string; icon: React.ElementType }[] = [
    { id: "runtimes", label: "Sandbox Runtimes", icon: Cpu },
    { id: "llm", label: "LLM Keys", icon: Key },
    { id: "github", label: "GitHub Apps", icon: Github },
    { id: "security", label: "Security", icon: Shield },
    { id: "integrations", label: "Integrations", icon: Zap },
    { id: "factories", label: "Factories", icon: Factory },
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
            <FactoriesSection hubUrl={hubPublicUrl} settings={settings} onSave={save} saving={saving} />
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

function LLMSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const providers = [
    { id: "anthropic", label: "Anthropic", placeholder: "sk-ant-..." },
  ]
  const [keys, setKeys] = useState<Record<string, string>>({})

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">LLM Keys</h2>
      <p className="text-sm text-muted-foreground mb-6">API keys injected into claws at bootstrap time.</p>
      <div className="space-y-4">
        {providers.map(({ id, label, placeholder }) => (
          <div key={id} className="border border-border rounded-lg p-4">
            <div className="flex items-center justify-between mb-3">
              <h3 className="font-medium text-sm">{label}</h3>
              {settings.llmKeys?.[id] && <span className="text-xs bg-green-500/20 text-green-400 px-2 py-1 rounded">Set</span>}
            </div>
            <div className="flex gap-2">
              <Input
                type="password"
                placeholder={settings.llmKeys?.[id] ? "••••••••••• (set)" : placeholder}
                value={keys[id] || ""}
                onChange={e => setKeys(prev => ({ ...prev, [id]: e.target.value }))}
                className="h-8 text-sm flex-1"
              />
              <Button size="sm" disabled={saving || !keys[id]} onClick={() => {
                onSave({ llmKeys: { [id]: keys[id] } })
                setKeys(prev => ({ ...prev, [id]: "" }))
              }}>
                Save
              </Button>
              {settings.llmKeys?.[id] && (
                <Button size="sm" variant="outline" disabled={saving} onClick={() => onSave({ llmKeys: { [id]: "" } })}>
                  Remove
                </Button>
              )}
            </div>
          </div>
        ))}
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
  const [editSecret, setEditSecret] = useState("")
  const [newWorkspace, setNewWorkspace] = useState("")
  const [newToken, setNewToken] = useState("")
  const [newSecret, setNewSecret] = useState("")

  const startEdit = (i: number) => {
    setEditingIdx(i)
    setEditWorkspace(linear[i].workspace)
    setEditToken("")
    setEditSecret("")
  }

  const saveEdit = () => {
    if (editingIdx === null) return
    const updated = linear.map((li, i) => i === editingIdx
      ? { workspace: editWorkspace, ...(editToken ? { token: editToken } : {}), ...(editSecret ? { webhookSecret: editSecret } : {}) }
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
                    <div>
                      <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret</label>
                      <Input type="password" value={editSecret} onChange={e => setEditSecret(e.target.value)} className="h-8 text-sm" placeholder="whsec_... (leave blank to keep)" />
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
                        Token: {li.tokenSet ? <span className="text-green-500">✓</span> : <span className="text-amber-500">✗</span>}
                        {" · Webhook secret: "}{li.webhookSecretSet ? <span className="text-green-500">✓</span> : <span className="text-amber-500">✗</span>}
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
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
            <Input type="password" value={newSecret} onChange={e => setNewSecret(e.target.value)} className="h-8 text-sm" placeholder="whsec_..." />
          </div>
          <Button size="sm" disabled={saving || !newWorkspace || !newToken}
            onClick={() => {
              onSave({ integrations: { linear: [...linear, { workspace: newWorkspace, token: newToken, ...(newSecret ? { webhookSecret: newSecret } : {}) }] } })
              setNewWorkspace(""); setNewToken(""); setNewSecret("")
            }}>
            Add Linear Workspace
          </Button>
        </div>
      </div>
    </div>
  )
}

interface FactoryFormData {
  name: string; workspace: string; team: string
  triggerStatus: string; doneStatus: string; terminateOnLeave: boolean; template: string
}

function FactoriesSection({ hubUrl, settings, onSave, saving }: { hubUrl: string; settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const factories = settings.factories || []
  const [copied, setCopied] = useState(false)
  const webhookUrl = hubUrl ? `${hubUrl}/api/integrations/linear/webhook` : ""
  const handleCopy = () => {
    if (!webhookUrl) return
    navigator.clipboard.writeText(webhookUrl).then(() => { setCopied(true); setTimeout(() => setCopied(false), 2000) })
  }
  const [form, setForm] = useState<FactoryFormData>({
    name: "", workspace: "", team: "", triggerStatus: "Ready for Agent",
    doneStatus: "In Review", terminateOnLeave: true, template: "base"
  })

  const workspaces = settings.integrations?.linear?.map(l => l.workspace) || []

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

      {/* Webhook URL */}
      <div className="border border-border rounded-lg p-5 space-y-3">
        <h3 className="text-sm font-medium">Linear Webhook URL</h3>
        <p className="text-xs text-muted-foreground">
          Paste into <strong>Linear → Settings → API → Webhooks</strong> and subscribe to <strong>Issue</strong> events.
        </p>
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

      {factories.length > 0 && (
        <div className="space-y-2 mb-6">
          {factories.map((f, i) => (
            <div key={i} className="border border-border rounded-lg p-4">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">{f.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {f.integration} · {f.team || f.workspace} · "{f.triggerStatus}" → template: {f.template}
                  </p>
                </div>
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive h-7 px-2" disabled={saving}
                  onClick={() => onSave({ factories: factories.filter((_, j) => j !== i) })}>
                  Remove
                </Button>
              </div>
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
            <label className="text-xs text-muted-foreground mb-1 block">Linear workspace</label>
            <select value={form.workspace} onChange={e => update("workspace", e.target.value)}
              className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm">
              <option value="">Select workspace</option>
              {workspaces.map(w => <option key={w} value={w}>{w}</option>)}
            </select>
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Team key</label>
            <Input value={form.team} onChange={e => update("team", e.target.value)} className="h-8 text-sm" placeholder="ELA" />
          </div>
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
        <Button size="sm" disabled={saving || !form.name || !form.workspace || !form.template || !form.triggerStatus}
          onClick={() => {
            onSave({ factories: [...factories, { ...form, integration: "linear" }] })
            setForm({ name: "", workspace: "", team: "", triggerStatus: "Ready for Agent", doneStatus: "In Review", terminateOnLeave: true, template: "base" })
          }}>
          Add Factory
        </Button>
      </div>
    </div>
  )
}
