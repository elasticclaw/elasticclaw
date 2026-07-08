"use client"

import { useState } from "react"
import { AlertTriangle, Check, CheckCircle2, ChevronLeft, ExternalLink, Github, RotateCcw, Shield, Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { useBranding } from "@/hooks/use-branding"
import type { GitHubAppView, SettingsData } from "./types"
import { useWorkspaceGitHubApps } from "./use-workspace-github-apps"

export default function GitHubSection({ settings, onSave, saving, workspace }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean; workspace: string }) {
  const { appName: brandName } = useBranding()
  const [showModal, setShowModal] = useState(false)
  const [appName, setAppName] = useState("")
  const [appId, setAppId] = useState("")
  const [url, setUrl] = useState("")
  const [installation, setInstallation] = useState("")
  const [pem, setPem] = useState("")
  const [testResult, setTestResult] = useState<GitHubAppView | null>(null)
  const [testing, setTesting] = useState(false)
  const [testError, setTestError] = useState("")
  const { workspaceApps, workspaceLoading, workspaceError, loadWorkspaceApps, saveWorkspaceApp, deleteWorkspaceApp } = useWorkspaceGitHubApps(workspace)
  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const resetModal = () => {
    setAppName(""); setAppId(""); setUrl(""); setInstallation(""); setPem("")
    setTestResult(null); setTestError(""); setTesting(false)
  }

  const openModal = () => { resetModal(); setShowModal(true) }
  const closeModal = () => { setShowModal(false); resetModal() }

  async function runTest() {
    setTesting(true); setTestError(""); setTestResult(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/github/test`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ appId: parseInt(appId, 10), url, privateKeyPem: pem }),
      })
      if (!res.ok) {
        const txt = await res.text()
        throw new Error(txt || `HTTP ${res.status}`)
      }
      setTestResult(await res.json())
    } catch (e) {
      setTestError(e instanceof Error ? e.message : "Test failed")
    } finally {
      setTesting(false)
    }
  }

  const [showConfirmModal, setShowConfirmModal] = useState(false)

  async function doSave(force = false) {
    // Not tested yet — recommend testing
    if (!force && testResult === null) {
      setShowConfirmModal(true)
      return
    }
    // Tested and failed — warn but allow
    if (!force && testResult && testResult.permCheckOk !== true) {
      setShowConfirmModal(true)
      return
    }
    const parsedAppId = parseInt(appId, 10)
    if (workspace) {
      const name = appName.trim() || `app-${parsedAppId}`
      const ok = await saveWorkspaceApp({ name, appId: parsedAppId, url, installation, privateKeyPem: pem })
      if (!ok) return
      closeModal()
      await loadWorkspaceApps()
      return
    }
    const newApp: { appId: number; privateKeyPem: string; url?: string } = { appId: parsedAppId, privateKeyPem: pem }
    if (url) newApp.url = url
    onSave({ github: [...(settings.github || []), newApp] })
    closeModal()
  }

  const needsAttention = testResult?.permissions?.filter(p => !p.ok).length ?? 0
  const configuredCount = testResult?.permissions?.filter(p => p.ok).length ?? 0
  const totalCount = testResult?.permissions?.length ?? 0

  return (
    <div>
      <h2 className="text-base font-semibold mb-1">GitHub Apps</h2>
      <div className="text-sm text-muted-foreground mb-6 space-y-1.5">
        <p>
          Register a GitHub App so your {brandName} workflows can access repositories.
          When an agent is created, it gets a scoped token that can read and write code,
          open pull requests, and check CI status — but only on repos the App is installed on.
        </p>
        <p>
          The App needs <strong>contents:write</strong>, <strong>pull_requests:write</strong>,
          and read access to <strong>metadata</strong>, <strong>checks</strong>, and <strong>statuses</strong>.
          Install it on your org or specific repos, then add the App ID and private key here.
        </p>
      </div>

      {workspaceError && <p className="mb-4 text-sm text-destructive">{workspaceError}</p>}

      {workspace ? (
        <div className="mb-6 space-y-2">
          {workspaceLoading ? (
            <p className="text-sm text-muted-foreground animate-pulse">Loading GitHub Apps...</p>
          ) : workspaceApps.length === 0 ? (
            <p className="text-sm text-muted-foreground">No GitHub Apps configured for this workspace.</p>
          ) : (
            workspaceApps.map(app => (
              <div key={app.name} className="border border-border rounded-lg p-4">
                <div className="flex items-center justify-between">
                  <div>
                    <p className="text-sm font-medium">{app.name}</p>
                    <p className="text-xs text-muted-foreground">App ID: {app.appId}</p>
                    {app.url && (
                      <a href={app.url} target="_blank" rel="noopener noreferrer" className="text-xs text-muted-foreground hover:text-primary flex items-center gap-1">
                        {app.url} <ExternalLink className="size-3" />
                      </a>
                    )}
                    {app.installation && <p className="text-xs text-muted-foreground">Installation: {app.installation}</p>}
                  </div>
                  <div className="flex items-center gap-2">
                    <span className={cn("text-xs px-2 py-1 rounded", (app.privateKeySet || app.private_key_set) ? "bg-green-500/20 text-green-400" : "bg-yellow-500/20 text-yellow-400")}>
                      {(app.privateKeySet || app.private_key_set) ? "Key set" : "No key"}
                    </span>
                    <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive h-7 px-2" disabled={saving}
                      onClick={() => deleteWorkspaceApp(app.name)}>
                      <Trash2 className="size-3.5" />
                    </Button>
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      ) : settings.github?.length > 0 && (
        <div className="mb-6 space-y-2">
          {settings.github.map(app => (
            <div key={app.appId} className="border border-border rounded-lg p-4">
              <div className="flex items-center justify-between mb-2">
                <div>
                  <p className="text-sm font-medium">App ID: {app.appId}</p>
                  {app.url && (
                    <a href={app.url} target="_blank" rel="noopener noreferrer" className="text-xs text-muted-foreground hover:text-primary flex items-center gap-1">
                      {app.url} <ExternalLink className="size-3" />
                    </a>
                  )}
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
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              </div>

              {/* Permission check results */}
              {app.permCheckError && (
                <div className="mt-3 rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-start gap-2">
                  <AlertTriangle className="size-4 text-yellow-400 shrink-0 mt-0.5" />
                  <div>
                    <p className="text-xs font-medium text-yellow-400">Permission check failed</p>
                    <p className="text-xs text-yellow-400/80">{app.permCheckError}</p>
                  </div>
                </div>
              )}
              {app.permissions && app.permissions.length > 0 && (
                <div className="mt-3">
                  {app.permCheckOk === false && (
                    <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 mb-2 flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <Shield className="size-4 text-yellow-400" />
                        <div>
                          <p className="text-xs font-medium text-yellow-400">
                            {app.permissions.filter(p => !p.ok).length} permission{app.permissions.filter(p => !p.ok).length !== 1 ? "s" : ""} need attention
                          </p>
                          <p className="text-xs text-yellow-400/70">
                            {app.permissions.filter(p => p.ok).length} of {app.permissions.length} required permissions granted
                          </p>
                        </div>
                      </div>
                      {app.url && (
                        <a href={app.url} target="_blank" rel="noopener noreferrer">
                          <Button size="sm" variant="outline" className="h-7 text-xs gap-1">
                            <ExternalLink className="size-3" /> Fix in GitHub
                          </Button>
                        </a>
                      )}
                    </div>
                  )}

                  <details className="group" open={app.permCheckOk === false}>
                    <summary className="flex items-center gap-2 text-xs font-medium text-muted-foreground cursor-pointer list-none">
                      <Shield className="size-3.5" />
                      Required Permissions
                      <ChevronLeft className="size-3 transition-transform group-open:-rotate-90" />
                    </summary>

                    {app.permissions.filter(p => !p.ok).length > 0 && (
                      <div className="mt-2 space-y-1">
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Needs Attention</p>
                        {app.permissions.filter(p => !p.ok).map(p => (
                          <div key={p.name} className="flex items-center justify-between rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2">
                            <div className="flex items-center gap-2">
                              <AlertTriangle className="size-3.5 text-yellow-400" />
                              <span className="text-sm font-mono text-yellow-400">{p.name}</span>
                            </div>
                            <div className="flex items-center gap-1.5 text-xs">
                              <span className="px-1.5 py-0.5 rounded bg-yellow-500/20 text-yellow-400">{p.granted || "not set"}</span>
                              <span className="text-muted-foreground">→</span>
                              <span className="px-1.5 py-0.5 rounded border border-yellow-500/30 text-yellow-400">needs {p.needed}</span>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}

                    {app.permissions.filter(p => p.ok).length > 0 && (
                      <div className="mt-2 space-y-1">
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Configured</p>
                        {app.permissions.filter(p => p.ok).map(p => (
                          <div key={p.name} className="flex items-center justify-between rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2">
                            <div className="flex items-center gap-2">
                              <CheckCircle2 className="size-3.5 text-green-400" />
                              <span className="text-sm font-mono text-green-400">{p.name}</span>
                            </div>
                            <span className="text-xs px-1.5 py-0.5 rounded bg-green-500/20 text-green-400">{p.granted}</span>
                          </div>
                        ))}
                      </div>
                    )}
                  </details>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Button onClick={openModal} className="gap-2">
        <Github className="size-4" /> Add GitHub App
      </Button>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { if (!open) closeModal(); else setShowModal(true) }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">Add GitHub App</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">Add GitHub App</h3>
          </div>

          <div className="p-5 space-y-4">
              <div className="grid grid-cols-2 gap-3">
                {workspace && (
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                    <Input value={appName} onChange={e => setAppName(e.target.value)} className="h-8 text-sm" placeholder="primary" />
                  </div>
                )}
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App ID</label>
                  <Input type="number" value={appId} onChange={e => setAppId(e.target.value)} className="h-8 text-sm" placeholder="123456" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">App URL (optional)</label>
                  <Input value={url} onChange={e => setUrl(e.target.value)} className="h-8 text-sm" placeholder="https://github.com/apps/..." />
                </div>
                {workspace && (
                  <div>
                    <label className="text-xs text-muted-foreground mb-1 block">Installation (optional)</label>
                    <Input value={installation} onChange={e => setInstallation(e.target.value)} className="h-8 text-sm" placeholder="owner or org" />
                  </div>
                )}
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

              <div className="flex gap-2">
                <Button size="sm" variant="outline" disabled={testing || !appId || !pem || isNaN(Number(appId))} onClick={runTest} className="gap-1">
                  {testing ? <RotateCcw className="size-3 animate-spin" /> : <Check className="size-3" />}
                  Test Permissions
                </Button>
              </div>

              {testError && (
                <div className="rounded-lg bg-red-500/10 border border-red-500/20 px-3 py-2.5 flex items-start gap-2">
                  <AlertTriangle className="size-4 text-red-400 shrink-0 mt-0.5" />
                  <p className="text-xs text-red-400">{testError}</p>
                </div>
              )}

              {/* Test result — show SOMETHING for every result state */}
              {testResult && (
                <div className="space-y-3">
                  {/* permCheckOk is true — all good */}
                  {testResult.permCheckOk === true && (
                    <div className="rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2.5 flex items-center gap-2">
                      <CheckCircle2 className="size-4 text-green-400" />
                      <div>
                        <p className="text-xs font-medium text-green-400">All {totalCount} required permissions granted</p>
                        <p className="text-xs text-green-400/70">This GitHub App is ready to use.</p>
                      </div>
                    </div>
                  )}

                  {/* permCheckOk is false or missing — show the problem */}
                  {(testResult.permCheckOk === false || testResult.permCheckOk == null) && (
                    <>
                      {testResult.permCheckError ? (
                        <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-start gap-2">
                          <AlertTriangle className="size-4 text-yellow-400 shrink-0 mt-0.5" />
                          <p className="text-xs text-yellow-400">{testResult.permCheckError}</p>
                        </div>
                      ) : (
                        <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2.5 flex items-center justify-between">
                          <div className="flex items-center gap-2">
                            <Shield className="size-4 text-yellow-400" />
                            <div>
                              <p className="text-xs font-medium text-yellow-400">
                                {needsAttention} permission{needsAttention !== 1 ? "s" : ""} need attention
                              </p>
                              <p className="text-xs text-yellow-400/70">
                                {configuredCount} of {totalCount} required permissions granted
                              </p>
                            </div>
                          </div>
                          {url && (
                            <a href={url} target="_blank" rel="noopener noreferrer">
                              <Button size="sm" variant="outline" className="h-7 text-xs gap-1">
                                <ExternalLink className="size-3" /> Fix in GitHub
                              </Button>
                            </a>
                          )}
                        </div>
                      )}
                    </>
                  )}

                  {/* Permission list — shown whenever we have permissions */}
                  {testResult.permissions && testResult.permissions.length > 0 && (
                    <div className="space-y-1">
                      {testResult.permissions.filter(p => !p.ok).length > 0 && (
                        <>
                          <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium">Needs Attention</p>
                          {testResult.permissions.filter(p => !p.ok).map(p => (
                            <div key={p.name} className="flex items-center justify-between rounded-lg bg-yellow-500/10 border border-yellow-500/20 px-3 py-2">
                              <div className="flex items-center gap-2">
                                <AlertTriangle className="size-3.5 text-yellow-400" />
                                <span className="text-sm font-mono text-yellow-400">{p.name}</span>
                              </div>
                              <div className="flex items-center gap-1.5 text-xs">
                                <span className="px-1.5 py-0.5 rounded bg-yellow-500/20 text-yellow-400">{p.granted || "not set"}</span>
                                <span className="text-muted-foreground">→</span>
                                <span className="px-1.5 py-0.5 rounded border border-yellow-500/30 text-yellow-400">needs {p.needed}</span>
                              </div>
                            </div>
                          ))}
                        </>
                      )}
                      {testResult.permissions.filter(p => p.ok).length > 0 && (
                        <>
                          <p className="text-[10px] uppercase tracking-wider text-muted-foreground/60 font-medium mt-2">Configured</p>
                          {testResult.permissions.filter(p => p.ok).map(p => (
                            <div key={p.name} className="flex items-center justify-between rounded-lg bg-green-500/10 border border-green-500/20 px-3 py-2">
                              <div className="flex items-center gap-2">
                                <CheckCircle2 className="size-3.5 text-green-400" />
                                <span className="text-sm font-mono text-green-400">{p.name}</span>
                              </div>
                              <span className="text-xs px-1.5 py-0.5 rounded bg-green-500/20 text-green-400">{p.granted}</span>
                            </div>
                          ))}
                        </>
                      )}
                    </div>
                  )}
                </div>
              )}
            </div>

            <div className="flex items-center justify-end gap-2 px-5 py-4 border-t border-border">
              <Button size="sm" variant="outline" onClick={closeModal}>Cancel</Button>
              <Button size="sm" disabled={saving || !appId || !pem || isNaN(Number(appId))} onClick={() => doSave(false)}>
                Save
              </Button>
            </div>
        </DialogContent>
      </Dialog>

      {/* Confirm save modal — not tested or test failed */}
      <Dialog open={showConfirmModal} onOpenChange={open => { if (!open) setShowConfirmModal(false) }}>
        <DialogContent className="max-w-md p-0 gap-0">
          <DialogTitle className="sr-only">Confirm GitHub App Save</DialogTitle>
          <div className="p-5 space-y-4">
            {testResult === null ? (
              <>
                <div className="flex items-center gap-2">
                  <AlertTriangle className="size-5 text-yellow-400" />
                  <h3 className="font-medium">Test Recommended</h3>
                </div>
                <p className="text-sm text-muted-foreground">
                  You haven&apos;t tested this GitHub App yet. We recommend clicking <strong>Test Permissions</strong> first to verify it works.
                </p>
              </>
            ) : (
              <>
                <div className="flex items-center gap-2">
                  <AlertTriangle className="size-5 text-yellow-400" />
                  <h3 className="font-medium">Permissions Missing</h3>
                </div>
                <p className="text-sm text-muted-foreground">
                  This GitHub App is missing required permissions. It <strong>will not work</strong> for agents until fixed.
                </p>
                {testResult.permCheckError && (
                  <p className="text-xs text-yellow-400 bg-yellow-500/10 rounded px-2 py-1.5">{testResult.permCheckError}</p>
                )}
              </>
            )}
            <div className="flex items-center justify-end gap-2 pt-2">
              <Button size="sm" variant="outline" onClick={() => setShowConfirmModal(false)}>Go Back</Button>
              <Button size="sm" variant="secondary" onClick={() => { setShowConfirmModal(false); doSave(true) }}>
                Save Anyway
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
