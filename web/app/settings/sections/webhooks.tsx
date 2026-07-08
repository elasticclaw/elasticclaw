"use client"

import { useState } from "react"
import { Check, Copy } from "lucide-react"
import { Button } from "@/components/ui/button"

// Currently unreferenced (kept from the original settings-content.tsx):
// per-workspace webhook URL reference card.
export default function WebhooksSection({ hubUrl, selectedWorkspace }: { hubUrl: string; selectedWorkspace: string }) {
  const [copied, setCopied] = useState<string | null>(null)
  const workspaceSlug = encodeURIComponent(selectedWorkspace)
  const workspaceWebhookBase = hubUrl ? `${hubUrl}/api/workspaces/${workspaceSlug}/webhooks` : ""

  const urls = [
    {
      name: "Linear",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/linear` : "",
      hint: "Paste into Linear → Settings → API → Webhooks, subscribe to Issue events.",
    },
    {
      name: "Shortcut",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/shortcut` : "",
      hint: "Use Shortcut's API to register this webhook: POST /api/v3/webhooks with this URL.",
    },
    {
      name: "GitHub Issues",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/github-issues` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Issues events.",
    },
    {
      name: "Jira",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/jira` : "",
      hint: "Use in Jira webhook settings. Subscribe to issue created and issue updated events.",
    },
    {
      name: "GitHub (PRs)",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/github` : "",
      hint: "Use in GitHub repo or org settings. Subscribe to: Pull requests and Issue comments.",
    },
    {
      name: "External",
      url: workspaceWebhookBase ? `${workspaceWebhookBase}/external` : "",
      hint: "Use for generic signed webhook events and release events.",
    },
  ]

  const doCopy = (text: string, label: string) => {
    if (!text) return
    navigator.clipboard.writeText(text).then(() => {
      setCopied(label)
      setTimeout(() => setCopied(null), 2000)
    })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Webhooks</h2>
        <p className="text-sm text-muted-foreground mb-6">
          Use these URLs to send events into the selected workspace.
        </p>
      </div>

      <div className="space-y-5">
        {urls.map(({ name, url, hint }) => (
          <div key={name} className="border border-border rounded-lg p-4 space-y-3">
            <div>
              <h4 className="text-sm font-medium">{name}</h4>
              <p className="text-xs text-muted-foreground mt-1">{hint}</p>
            </div>
            <div className="flex items-center gap-2">
              <code className="flex-1 text-xs font-mono bg-muted px-3 py-2 rounded-md border border-border truncate">
                {url || "Loading…"}
              </code>
              <Button variant="outline" size="sm" className="shrink-0" onClick={() => doCopy(url, name)} disabled={!url}>
                {copied === name ? <Check className="size-3.5 text-green-500" /> : <Copy className="size-3.5" />}
                <span className="ml-1.5">{copied === name ? "Copied" : "Copy"}</span>
              </Button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
