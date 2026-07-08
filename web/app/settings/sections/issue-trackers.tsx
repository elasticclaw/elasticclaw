"use client"

import { useEffect, useRef, useState } from "react"
import { AlertTriangle, Check, CheckCircle2, Copy, Github, Trash2, Zap } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { useBranding } from "@/hooks/use-branding"
import type { SettingsData } from "./types"
import { useIssueTrackers, type TrackerItem, type TrackerType } from "./use-issue-trackers"

export default function IntegrationsSection({ selectedWorkspace, hubPublicUrl, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean; selectedWorkspace: string; hubPublicUrl: string }) {
  const { appName } = useBranding()
  const { workspaceTrackers, githubOwnerHint, loading, error, setError, loadTrackers, issueTrackersPath } = useIssueTrackers(selectedWorkspace)
  const hubUrl = getHubUrl()
  const authToken = () => getAuthToken() || ""
  const linear = workspaceTrackers.filter(t => t.type === "linear")
  const shortcut = workspaceTrackers.filter(t => t.type === "shortcut")
  const githubIssues = workspaceTrackers.filter(t => t.type === "github-issues")
  const jira = workspaceTrackers.filter(t => t.type === "jira")

  const allTrackers: TrackerItem[] = workspaceTrackers

  // Unified modal state
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [modalType, setModalType] = useState<TrackerType>("linear")
  const [editIdx, setEditIdx] = useState<number | null>(null)
  const [editType, setEditType] = useState<TrackerType>("linear")
  const [baseUrl, setBaseUrl] = useState("")
  const [username, setUsername] = useState("")
  const [token, setToken] = useState("")
  const [webhookSecret, setWebhookSecret] = useState("")
  const [copiedSetup, setCopiedSetup] = useState<string | null>(null)
  const [setupTab, setSetupTab] = useState<"token" | "webhook">("token")
  const [showAddMenu, setShowAddMenu] = useState(false)
  const addMenuRef = useRef<HTMLDivElement>(null)

  // Close add menu on outside click
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (addMenuRef.current && !addMenuRef.current.contains(e.target as Node)) {
        setShowAddMenu(false)
      }
    }
    if (showAddMenu) {
      document.addEventListener("mousedown", handleClick)
      return () => document.removeEventListener("mousedown", handleClick)
    }
  }, [showAddMenu])

  const resetModal = () => {
    setBaseUrl(""); setUsername(""); setToken(""); setWebhookSecret(""); setEditIdx(null); setEditType("linear"); setSetupTab("token")
  }

  const openAdd = (type: TrackerType) => {
    resetModal()
    setModalType(type)
    setModalMode("add")
    setShowModal(true)
    setShowAddMenu(false)
  }

  const openEdit = (tracker: TrackerItem, idx: number) => {
    setToken("")
    setBaseUrl(tracker.baseUrl || "")
    setUsername(tracker.username || "")
    setWebhookSecret("")
    setEditIdx(idx)
    setEditType(tracker.type)
    setSetupTab("token")
    setModalMode("edit")
    setShowModal(true)
  }

  async function saveTracker() {
    if (modalMode === "add" && !token.trim()) return

    const type = modalMode === "add" ? modalType : editType
    if (type === "jira" && !baseUrl.trim()) {
      setError("Jira base URL is required")
      return
    }
    const trackerWorkspace = selectedWorkspace
    const originalWorkspace = modalMode === "edit" && editIdx !== null ? allTrackers[editIdx]?.workspace : ""
    setError("")
    const res = await fetch(`${hubUrl}${issueTrackersPath}`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${authToken()}`, "Content-Type": "application/json" },
      body: JSON.stringify({ type, workspace: trackerWorkspace, originalWorkspace, baseUrl: baseUrl.trim(), username: username.trim(), token: token.trim(), webhookSecret: webhookSecret.trim() }),
    })
    if (!res.ok) {
      setError(await res.text())
      return
    }
    setShowModal(false)
    resetModal()
    await loadTrackers()
  }

  async function removeTracker() {
    if (editIdx === null) return
    const tracker = allTrackers[editIdx]
    if (!tracker) return
    setError("")
    const res = await fetch(`${hubUrl}${issueTrackersPath}?type=${encodeURIComponent(tracker.type)}&workspace=${encodeURIComponent(tracker.workspace)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${authToken()}` },
    })
    if (!res.ok) {
      setError(await res.text())
      return
    }
    setShowModal(false)
    resetModal()
    await loadTrackers()
  }

  const trackerTypeLabel = (t: TrackerType) => {
    switch (t) {
      case "linear": return "Linear"
      case "shortcut": return "Shortcut"
      case "github-issues": return "GitHub Issues"
      case "jira": return "Jira"
    }
  }

  const modalTitle = modalMode === "add"
    ? `Add ${trackerTypeLabel(modalType)}`
    : `Edit ${trackerTypeLabel(editType)}`

  const modalIcon = (modalMode === "add" ? modalType : editType) === "linear"
    ? <Zap className="size-4" />
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <span className="text-[#F4603C]">⚡</span>
    : (modalMode === "add" ? modalType : editType) === "jira"
    ? <span className="text-[#0052CC] font-semibold text-sm">J</span>
    : <Github className="size-4" />

  const githubIssuesTokenParams = new URLSearchParams({
    name: `${appName} GitHub Issues`,
    description: `Allows ${appName} to read and update GitHub issues`,
    expires_in: "90",
    issues: "write",
    metadata: "read",
  })
  if (githubOwnerHint) {
    githubIssuesTokenParams.set("target_name", githubOwnerHint)
  }
  const githubIssuesTokenUrl = `https://github.com/settings/personal-access-tokens/new?${githubIssuesTokenParams.toString()}`
  const tokenHint = (modalMode === "add" ? modalType : editType) === "linear"
    ? <>Use a Linear API key from <a href="https://linear.app/settings/account/security" target="_blank" rel="noopener noreferrer" className="underline">linear.app/settings/account/security</a>.</>
    : (modalMode === "add" ? modalType : editType) === "shortcut"
    ? <>Use a Shortcut API token from Shortcut settings. The token lets {appName} read and update stories.</>
    : (modalMode === "add" ? modalType : editType) === "github-issues"
    ? <>Use a <a href={githubIssuesTokenUrl} target="_blank" rel="noopener noreferrer" className="underline">fine-grained GitHub PAT</a> for issue API actions. {githubOwnerHint ? <>The link starts with <code>{githubOwnerHint}</code> as the resource owner. </> : null}Grant repository access to the repos this workspace watches.</>
    : (modalMode === "add" ? modalType : editType) === "jira"
    ? <>Use a Jira personal access token or API token with issue read/write permissions.</>
    : null
  const activeTrackerType = modalMode === "add" ? modalType : editType
  const canGenerateWebhookSecret = activeTrackerType === "github-issues" || activeTrackerType === "shortcut" || activeTrackerType === "jira"
  const workspaceWebhookBase = hubPublicUrl || hubUrl
  const linearWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/linear`
  const githubIssuesWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/github-issues`
  const jiraWebhookUrl = `${workspaceWebhookBase}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/webhooks/jira`

  function generateWebhookSecret() {
    const bytes = new Uint8Array(32)
    crypto.getRandomValues(bytes)
    setWebhookSecret(Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join(""))
  }

  function copySetupValue(value: string, key: string) {
    navigator.clipboard.writeText(value).then(() => {
      setCopiedSetup(key)
      setTimeout(() => setCopiedSetup(null), 2000)
    })
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Issue Trackers</h2>
        <p className="text-sm text-muted-foreground mb-4">Connect issue trackers to sync and create issues from workflows.</p>

        {/* Summary badges */}
        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {allTrackers.length} tracker{allTrackers.length !== 1 ? "s" : ""} connected
          </span>
          {linear.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Linear: {linear.length}</span>
          )}
          {shortcut.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Shortcut: {shortcut.length}</span>
          )}
          {githubIssues.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">GitHub Issues: {githubIssues.length}</span>
          )}
          {jira.length > 0 && (
            <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">Jira: {jira.length}</span>
          )}
        </div>
      </div>

      {error && <p className="text-sm text-destructive">{error}</p>}

      {/* Configured trackers list */}
      {loading ? (
        <p className="text-sm text-muted-foreground animate-pulse">Loading issue trackers...</p>
      ) : allTrackers.length > 0 && (
        <div className="space-y-2 mb-4">
          {allTrackers.map((tracker, i) => (
            <div
              key={`${tracker.type}-${tracker.workspace}`}
              onClick={() => openEdit(tracker, i)}
              className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
            >
              <div className="flex items-center gap-3">
                {tracker.type === "linear" ? (
                  <Zap className="size-4 text-muted-foreground" />
                ) : tracker.type === "shortcut" ? (
                  <span className="text-[#F4603C]">⚡</span>
                ) : tracker.type === "jira" ? (
                  <span className="text-[#0052CC] font-semibold text-sm">J</span>
                ) : (
                  <Github className="size-4 text-muted-foreground" />
                )}
                <div>
                  <p className="text-sm font-medium">{tracker.workspace}</p>
                  <div className="flex items-center gap-1.5 mt-0.5">
                    <span className="text-xs text-muted-foreground capitalize">{tracker.type === "github-issues" ? "github issues" : tracker.type}</span>
                    {tracker.type === "jira" && tracker.baseUrl ? (
                      <>
                        <span className="text-xs text-muted-foreground">·</span>
                        <span className="text-xs text-muted-foreground">{tracker.baseUrl}</span>
                      </>
                    ) : null}
                    <span className="text-xs text-muted-foreground">·</span>
                    {tracker.tokenSet ? (
                      <span className="text-xs text-green-400 flex items-center gap-1">
                        <CheckCircle2 className="size-3" /> Connected
                      </span>
                    ) : (
                      <span className="text-xs text-red-400 flex items-center gap-1">
                        <AlertTriangle className="size-3" /> Token revoked
                      </span>
                    )}
                  </div>
                </div>
              </div>
              <span className="text-muted-foreground text-lg">⋯</span>
            </div>
          ))}
        </div>
      )}

      {!loading && allTrackers.length === 0 && (
        <div className="border border-dashed border-border rounded-lg p-8 text-center space-y-3 mb-4">
          <div className="w-10 h-10 rounded-full bg-muted flex items-center justify-center mx-auto">
            <Zap className="size-5 text-muted-foreground" />
          </div>
          <p className="text-sm text-muted-foreground">No issue trackers connected</p>
        </div>
      )}

      {/* Add dropdown */}
      <div className="relative" ref={addMenuRef}>
        <Button size="sm" variant="outline" onClick={() => setShowAddMenu(!showAddMenu)} className="gap-1">
          <span className="text-sm">+</span> Add issue tracker
        </Button>
        {showAddMenu && (
          <div className="absolute top-full left-0 mt-1 bg-background border border-border rounded-lg shadow-lg py-1 z-50 min-w-[180px]">
            <button
              onClick={() => openAdd("linear")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <Zap className="size-4" />
              <span>Linear</span>
            </button>
            <button
              onClick={() => openAdd("shortcut")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <span className="text-[#F4603C]">⚡</span>
              <span>Shortcut</span>
            </button>
            <button
              onClick={() => openAdd("github-issues")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <Github className="size-4" />
              <span>GitHub Issues</span>
            </button>
            <button
              onClick={() => openAdd("jira")}
              className="w-full flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted transition-colors text-left"
            >
              <span className="text-[#0052CC] font-semibold text-sm">J</span>
              <span>Jira</span>
            </button>
          </div>
        )}
      </div>

      {/* Unified Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetModal() }}>
        <DialogContent className={cn("p-0 gap-0", activeTrackerType === "github-issues" || activeTrackerType === "linear" ? "sm:max-w-5xl w-[min(1100px,calc(100vw-2rem))]" : "max-w-lg")}>
          <DialogTitle className="sr-only">{modalTitle}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <div className="flex items-center gap-2">
              {modalIcon}
              <h3 className="font-medium">{modalTitle}</h3>
            </div>
          </div>
          <div className="p-5 space-y-4">
              {activeTrackerType === "github-issues" || activeTrackerType === "linear" ? (
                <div className="grid min-h-[420px] grid-cols-[240px_1fr]">
                  <div className="-ml-5 -my-5 border-r border-border p-4">
                    <div className="space-y-1">
                      <button
                        type="button"
                        onClick={() => setSetupTab("token")}
                        className={cn(
                          "w-full rounded-lg px-3 py-2 text-left text-sm transition-colors",
                          setupTab === "token" ? "bg-primary/10 text-primary font-medium" : "text-muted-foreground hover:bg-secondary hover:text-foreground"
                        )}
                      >
                        {activeTrackerType === "github-issues" ? "GitHub PAT" : "API Token"}
                      </button>
                      <button
                        type="button"
                        onClick={() => setSetupTab("webhook")}
                        className={cn(
                          "w-full rounded-lg px-3 py-2 text-left text-sm transition-colors",
                          setupTab === "webhook" ? "bg-primary/10 text-primary font-medium" : "text-muted-foreground hover:bg-secondary hover:text-foreground"
                        )}
                      >
                        {activeTrackerType === "github-issues" ? "Webhook" : "Webhook Secret"}
                      </button>
                    </div>
                  </div>
                  <div className="pl-5">
                    <p className="text-sm text-muted-foreground">
                      {activeTrackerType === "github-issues"
                        ? "Connect GitHub Issues for this workspace. This is separate from the GitHub App used for repo checkout tokens."
                        : `Connect Linear for this workspace. The API key lets ${appName} read issues and move them between statuses.`}
                    </p>
                    {setupTab === "token" ? (
                    <div className="mt-4 space-y-4">
                      <div>
                        <h4 className="text-sm font-medium">{activeTrackerType === "github-issues" ? "GitHub PAT" : "Linear API Token"}</h4>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? `Used by ${appName} to read and update issues.`
                            : `Used by ${appName} to read Linear issues and move them between statuses.`}
                        </p>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                        <Input type="password" value={token} onChange={e => setToken(e.target.value)} className="h-9 text-sm" placeholder={`${trackerTypeLabel(activeTrackerType)} API token`} />
                        {tokenHint && <p className="text-xs text-muted-foreground mt-1">{tokenHint}</p>}
                      </div>
                    </div>
                    ) : (
                    <div className="mt-4 space-y-4">
                      <div>
                        <h4 className="text-sm font-medium">Webhook</h4>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? "Create a repo or org webhook for Issues events."
                            : "Create a Linear webhook for Issue events, then paste its signing secret below if you configured one."}
                        </p>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">Payload URL</label>
                        <div className="flex gap-2">
                          <Input readOnly value={activeTrackerType === "github-issues" ? githubIssuesWebhookUrl : linearWebhookUrl} className="h-9 text-xs font-mono" />
                          <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(activeTrackerType === "github-issues" ? githubIssuesWebhookUrl : linearWebhookUrl, `${activeTrackerType}-url`)}>
                            {copiedSetup === `${activeTrackerType}-url` ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                        </div>
                      </div>
                      <div>
                        <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
                        <div className="flex gap-2">
                          <Input type="password" value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)} className="h-9 text-sm" placeholder="Webhook secret" />
                          <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(webhookSecret, `${activeTrackerType}-secret`)} disabled={!webhookSecret}>
                            {copiedSetup === `${activeTrackerType}-secret` ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                          {activeTrackerType === "github-issues" && (
                            <Button type="button" size="sm" variant="outline" onClick={generateWebhookSecret}>
                              Generate
                            </Button>
                          )}
                        </div>
                        <p className="text-xs text-muted-foreground mt-1">
                          {activeTrackerType === "github-issues"
                            ? "Paste the same secret into the GitHub webhook Secret field."
                            : "Copy the signing secret from the Linear webhook settings and paste it here. Leave blank only if you intentionally want unsigned Linear webhooks."}
                        </p>
                      </div>
                    </div>
                    )}
                  </div>
                </div>
              ) : activeTrackerType === "shortcut" ? (
                <div className="space-y-2 text-sm text-muted-foreground">
                  <p>Connect Shortcut for this workspace. The API token lets {appName} read stories and update workflow states.</p>
                  <p>Create a Shortcut webhook using the Shortcut URL from the Webhooks page. If Shortcut signs the payload with a secret, paste that same secret below.</p>
                </div>
              ) : activeTrackerType === "jira" ? (
                <div className="space-y-2 text-sm text-muted-foreground">
                  <p>Connect Jira for this workspace. The token lets {appName} read issues, add comments, and transition statuses.</p>
                  <p>Create a Jira Automation rule that sends a web request with the Issue data automation payload, then use the payload URL below.</p>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">
                  Connect {trackerTypeLabel(activeTrackerType)} for this workspace.
                </p>
              )}
              {activeTrackerType !== "github-issues" && activeTrackerType !== "linear" && (
                <>
              {activeTrackerType === "jira" && (
                <>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Jira Base URL</label>
                    <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)} className="h-9 text-sm" placeholder="https://jira.example.com" />
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Username <span className="text-muted-foreground/60">(optional)</span></label>
                    <Input value={username} onChange={e => setUsername(e.target.value)} className="h-9 text-sm" placeholder="admin@example.com" />
                    <p className="text-xs text-muted-foreground mt-1">Set for basic auth. Leave blank to send the token as a bearer token.</p>
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Payload URL</label>
                    <div className="flex gap-2">
                      <Input readOnly value={jiraWebhookUrl} className="h-9 text-xs font-mono" />
                      <Button type="button" size="sm" variant="outline" onClick={() => copySetupValue(jiraWebhookUrl, "jira-url")}>
                        {copiedSetup === "jira-url" ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                      </Button>
                    </div>
                    <p className="text-xs text-muted-foreground mt-1">Use this URL from Jira Automation&apos;s Send web request action with the Issue data automation payload.</p>
                  </div>
                </>
              )}
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                <Input type="password" value={token} onChange={e => setToken(e.target.value)} className="h-9 text-sm" placeholder={`${trackerTypeLabel(activeTrackerType)} API token`} />
                {tokenHint && <p className="text-xs text-muted-foreground mt-1">{tokenHint}</p>}
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Webhook Secret <span className="text-muted-foreground/60">(optional)</span></label>
                <div className="flex gap-2">
                  <Input type="password" value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)} className="h-9 text-sm" placeholder="Webhook secret for signature verification" />
                  {canGenerateWebhookSecret && (
                    <Button type="button" size="sm" variant="outline" onClick={generateWebhookSecret}>
                      Generate
                    </Button>
                  )}
                </div>
                <p className="text-xs text-muted-foreground mt-1">
                  {activeTrackerType === "shortcut"
                    ? "Generate one here, then use the same value when configuring the Shortcut webhook signature secret."
                    : activeTrackerType === "jira"
                    ? "Generate one here, then send it in the X-ElasticClaw-Webhook-Secret header."
                    : "Used to verify incoming webhook signatures. Leave blank to keep existing."}
                </p>
              </div>
                </>
              )}
            </div>
            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={removeTracker}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetModal() }}>Cancel</Button>
                <Button size="sm" disabled={saving || (modalMode === "add" && !token.trim()) || (activeTrackerType === "jira" && !baseUrl.trim())} onClick={saveTracker}>
                  {modalMode === "add" ? "Add tracker" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
