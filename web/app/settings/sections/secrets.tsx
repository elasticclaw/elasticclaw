"use client"

import { useCallback, useEffect, useState } from "react"
import { Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import type { SettingsData } from "./types"

export default function SecretsSection({ workspace }: { settings: SettingsData | null; workspace: string }) {
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
