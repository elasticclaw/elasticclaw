"use client"

import { useState, useEffect, useCallback } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { Cpu, Key, Github, ChevronLeft, Shield } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

type Section = "runtimes" | "llm" | "github" | "security"

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
      .then((d) => { setVersion(d.version || "unknown") })
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
  ]

  return (
    <div className="min-h-screen bg-background flex flex-col">
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