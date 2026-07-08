"use client"

import { useState } from "react"
import { Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import type { SettingsData } from "./types"

export default function MCPServersSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => Promise<boolean>; saving: boolean }) {
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
