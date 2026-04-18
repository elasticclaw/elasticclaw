"use client"

import { useState, useEffect } from "react"
import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { saveConfig, clearConfig, isConfigured } from "@/lib/api"
import { ArrowLeft, Save, Trash2 } from "lucide-react"

export default function SettingsPage() {
  const router = useRouter()
  const [hubUrl, setHubUrl] = useState("")
  const [hubToken, setHubToken] = useState("")
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    const url =
      process.env.NEXT_PUBLIC_HUB_URL ||
      (typeof window !== "undefined" ? localStorage.getItem("hub_url") || "" : "")
    const token =
      process.env.NEXT_PUBLIC_HUB_TOKEN ||
      (typeof window !== "undefined" ? localStorage.getItem("hub_token") || "" : "")
    setHubUrl(url)
    setHubToken(token)
  }, [])

  const handleSave = () => {
    saveConfig(hubUrl.trim(), hubToken.trim())
    setSaved(true)
    setTimeout(() => {
      router.push("/")
    }, 800)
  }

  const handleClear = () => {
    clearConfig()
    setHubUrl("")
    setHubToken("")
  }

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="w-full max-w-md p-8 space-y-6">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="icon" onClick={() => router.push("/")}>
            <ArrowLeft className="size-4" />
          </Button>
          <h1 className="text-xl font-semibold">Settings</h1>
        </div>

        <div className="space-y-4">
          <Field>
            <FieldLabel htmlFor="hub-url">Hub URL</FieldLabel>
            <Input
              id="hub-url"
              placeholder="http://your-hub-host:8080"
              value={hubUrl}
              onChange={(e) => setHubUrl(e.target.value)}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="hub-token">Hub Token</FieldLabel>
            <Input
              id="hub-token"
              type="password"
              placeholder="your-auth-token"
              value={hubToken}
              onChange={(e) => setHubToken(e.target.value)}
            />
          </Field>
        </div>

        <div className="flex items-center gap-3">
          <Button
            onClick={handleSave}
            disabled={!hubUrl.trim() || !hubToken.trim()}
            className="flex-1"
          >
            <Save className="size-4 mr-2" />
            {saved ? "Saved!" : "Save & Connect"}
          </Button>
          <Button variant="outline" size="icon" onClick={handleClear}>
            <Trash2 className="size-4" />
          </Button>
        </div>

        <p className="text-xs text-muted-foreground">
          These settings are stored in localStorage. You can also set{" "}
          <code className="bg-muted px-1 rounded text-xs">NEXT_PUBLIC_HUB_URL</code> and{" "}
          <code className="bg-muted px-1 rounded text-xs">NEXT_PUBLIC_HUB_TOKEN</code>{" "}
          environment variables instead.
        </p>
      </div>
    </div>
  )
}
