"use client"

import { useState } from "react"
import { CheckCircle2, Cpu } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { SettingsData } from "./types"
import { ProviderModal, SANDBOX_PROVIDER_OPTIONS } from "./runtimes-provider-modal"

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

export default function RuntimesSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
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

  const openAdd = () => {
    setEditName(null)
    setModalMode("add")
    setShowModal(true)
  }
  const openEdit = (name: string) => {
    setEditName(name)
    setModalMode("edit")
    setShowModal(true)
  }
  const closeModal = () => {
    setShowModal(false)
    setEditName(null)
  }

  const availableProviders = SANDBOX_PROVIDER_OPTIONS.filter(o => !providers[o.value])

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

      <ProviderModal
        open={showModal}
        mode={modalMode}
        editName={editName}
        providers={providers}
        saving={saving}
        onSave={onSave}
        onClose={closeModal}
      />
    </div>
  )
}
