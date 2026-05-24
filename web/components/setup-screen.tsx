"use client"

import { useState } from "react"
import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { saveConfig } from "@/lib/api"

export function SetupScreen({ onConnected }: { onConnected: () => void }) {
  const [hubUrl, setHubUrl] = useState("")
  const [hubToken, setHubToken] = useState("")
  const [error, setError] = useState("")

  const handleConnect = () => {
    if (!hubUrl.trim() || !hubToken.trim()) {
      setError("Both Hub URL and Token are required")
      return
    }
    saveConfig(hubUrl.trim(), hubToken.trim())
    onConnected()
  }

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="w-full max-w-md p-8 space-y-6">
        <div className="space-y-2">
          <h1 className="text-2xl font-bold">Connect to Hub</h1>
          <p className="text-sm text-muted-foreground">
            Enter your ElasticClaw Server URL and authentication token to get started.
          </p>
        </div>

        <div className="space-y-4">
          <Field>
            <FieldLabel htmlFor="hub-url">Hub URL</FieldLabel>
            <Input
              id="hub-url"
              placeholder="http://your-hub-host:8080"
              value={hubUrl}
              onChange={(e) => setHubUrl(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && handleConnect()}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="hub-token">Token</FieldLabel>
            <Input
              id="hub-token"
              type="password"
              placeholder="your-auth-token"
              value={hubToken}
              onChange={(e) => setHubToken(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && handleConnect()}
            />
          </Field>
          {error && <p className="text-sm text-destructive">{error}</p>}
        </div>

        <Button
          className="w-full"
          onClick={handleConnect}
          disabled={!hubUrl.trim() || !hubToken.trim()}
        >
          Connect
        </Button>

        <p className="text-xs text-muted-foreground text-center">
          You can also set <code className="bg-muted px-1 rounded">NEXT_PUBLIC_HUB_URL</code> and{" "}
          <code className="bg-muted px-1 rounded">NEXT_PUBLIC_HUB_TOKEN</code> environment variables.
        </p>
      </div>
    </div>
  )
}
