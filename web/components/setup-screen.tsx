"use client"

import { useState } from "react"
import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { saveConfig } from "@/lib/api"
import { useBranding } from "@/hooks/use-branding"

export function SetupScreen({ onConnected }: { onConnected: () => void }) {
  const { appName } = useBranding()
  const [hubUrl, setHubUrl] = useState("")
  const [hubToken, setHubToken] = useState("")
  const [error, setError] = useState("")

  const handleConnect = () => {
    if (!hubUrl.trim() || !hubToken.trim()) {
      setError(`Both ${appName} Server URL and token are required`)
      return
    }
    saveConfig(hubUrl.trim(), hubToken.trim())
    onConnected()
  }

  return (
    <div className="grid h-screen place-items-center bg-background p-4">
      <div className="w-full max-w-[440px] space-y-6">
        <div>
          <h1 className="text-[26px]">Connect to {appName} Server</h1>
          <p className="mt-2 text-sm text-muted-foreground">
            Enter your {appName} Server URL and authentication token to get started.
          </p>
        </div>

        <div className="space-y-4">
          <Field className="gap-1.5">
            <FieldLabel htmlFor="hub-url">{appName} Server URL</FieldLabel>
            <Input
              id="hub-url"
              placeholder="http://your-server-host:8080"
              value={hubUrl}
              onChange={(e) => setHubUrl(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && handleConnect()}
            />
          </Field>
          <Field className="gap-1.5">
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
          className="w-full justify-start"
          onClick={handleConnect}
          disabled={!hubUrl.trim() || !hubToken.trim()}
        >
          Connect
        </Button>

        <p className="text-xs text-muted-foreground">
          You can also set{" "}
          <code className="rounded-sm bg-foreground/10 px-1.5 py-px font-mono text-[0.92em] text-foreground">
            NEXT_PUBLIC_HUB_URL
          </code>{" "}
          and{" "}
          <code className="rounded-sm bg-foreground/10 px-1.5 py-px font-mono text-[0.92em] text-foreground">
            NEXT_PUBLIC_HUB_TOKEN
          </code>{" "}
          environment variables.
        </p>
      </div>
    </div>
  )
}
