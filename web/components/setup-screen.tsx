"use client"

import { useState } from "react"
import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { saveConfig } from "@/lib/api"
import { useBranding } from "@/hooks/use-branding"
import { Badge } from "@/components/ui/badge"
import { Blueprint } from "@/components/ui/blueprint"
import { Kicker } from "@/components/ui/kicker"

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

  const hasHubUrl = !!hubUrl.trim()
  const hasHubToken = !!hubToken.trim()

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="grid w-full max-w-[400px] gap-5 px-6">
        <div className="grid gap-1.5">
          <Kicker emphasis>First run</Kicker>
          <h2 className="text-[30px]">Connect a hub</h2>
          <p className="text-[13px] text-muted-foreground text-pretty">
            Enter your {appName} Server URL and authentication token to get started.
          </p>
        </div>

        <Blueprint className="grid gap-4 p-[22px]">
          <div className="grid gap-4">
            <Field>
              <FieldLabel htmlFor="hub-url">{appName} Server URL</FieldLabel>
              <Input
                id="hub-url"
                placeholder="http://your-server-host:8080"
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
          </div>

          <div className="grid gap-2.5">
            <div className="flex items-center gap-2.5 text-[13px]">
              <span className="grid size-[18px] shrink-0 place-items-center border border-primary font-mono text-[11px] text-accent-foreground">1</span>
              <span className="flex-1">Hub address</span>
              {hasHubUrl ? <Badge variant="outline">chosen</Badge> : <Badge variant="neutral">missing</Badge>}
            </div>
            <div className="flex items-center gap-2.5 text-[13px]">
              <span className={`grid size-[18px] shrink-0 place-items-center border font-mono text-[11px] ${hasHubUrl ? "border-primary text-accent-foreground" : "border-border text-muted-foreground"}`}>2</span>
              <span className={`flex-1 ${hasHubUrl ? "text-foreground" : "text-muted-foreground"}`}>Access token</span>
              {hasHubToken ? <Badge variant="outline">chosen</Badge> : <Badge variant="neutral">missing</Badge>}
            </div>
            {hasHubUrl && hasHubToken && (
              <div className="flex items-center gap-2.5 text-[13px]">
                <span className="grid size-[18px] shrink-0 place-items-center border border-primary font-mono text-[11px] text-accent-foreground">3</span>
                <span className="flex-1">Ready to connect</span>
                <Badge variant="accent">ok</Badge>
              </div>
            )}
          </div>

          {error && <p className="text-sm text-destructive">{error}</p>}

          <Button
            className="w-full"
            onClick={handleConnect}
            disabled={!hubUrl.trim() || !hubToken.trim()}
          >
            Connect
          </Button>
        </Blueprint>

        <p className="text-xs text-muted-foreground text-center">
          You can also set <code className="font-mono">NEXT_PUBLIC_HUB_URL</code> and{" "}
          <code className="font-mono">NEXT_PUBLIC_HUB_TOKEN</code> environment variables.
        </p>
      </div>
    </div>
  )
}
