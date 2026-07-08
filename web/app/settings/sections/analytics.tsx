"use client"

import { useCallback, useEffect, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { fetchWorkspaces, type Workspace } from "@/lib/api"

interface AnalyticsSummary {
  factoryName: string
  totalTriggers: number
  successfulCreations: number
  failedCreations: number
  terminations: number
  prOpened: number
  prMerged: number
  prClosed: number
  doneSignals: number
  errors: number
  successRate: number
  prMergeRate: number
}

export default function AnalyticsSection({ selectedWorkspace }: { selectedWorkspace?: string }) {
  const [summaries, setSummaries] = useState<AnalyticsSummary[]>([])
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const hubUrl = getHubUrl()
      const token = getAuthToken() || ""
      const [analyticsRes, workspaceData] = await Promise.all([
        fetch(`${hubUrl}/api/analytics/factories`, { headers: { Authorization: `Bearer ${token}` } }),
        fetchWorkspaces(),
      ])
      if (!analyticsRes.ok) throw new Error(await analyticsRes.text())
      setSummaries(await analyticsRes.json())
      setWorkspaces(workspaceData)
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load analytics")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const scopedToWorkspace = selectedWorkspace !== undefined
  const workflowNames = new Set(
    scopedToWorkspace && selectedWorkspace
      ? workspaces.find((workspace) => workspace.name === selectedWorkspace)?.workflows.map((workflow) => workflow.name) || []
      : []
  )
  const visibleSummaries = scopedToWorkspace
    ? summaries.filter((summary) => workflowNames.has(summary.factoryName))
    : summaries
  const totalTriggers = visibleSummaries.reduce((sum, item) => sum + item.totalTriggers, 0)
  const successfulCreations = visibleSummaries.reduce((sum, item) => sum + item.successfulCreations, 0)
  const failedCreations = visibleSummaries.reduce((sum, item) => sum + item.failedCreations, 0)
  const prMerged = visibleSummaries.reduce((sum, item) => sum + item.prMerged, 0)

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Analytics</h2>
        <p className="text-sm text-muted-foreground">
          {scopedToWorkspace ? "Activity for workflows in this workspace." : "Activity across all workspaces."}
        </p>
      </div>

      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading analytics…</p>
      ) : visibleSummaries.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No analytics yet.</p>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-4">
            <AnalyticsMetric label="Triggers" value={totalTriggers} />
            <AnalyticsMetric label="Created" value={successfulCreations} />
            <AnalyticsMetric label="Failed" value={failedCreations} />
            <AnalyticsMetric label="PRs merged" value={prMerged} />
          </div>
          <div className="border border-border rounded-lg divide-y divide-border">
            {visibleSummaries.map((summary) => (
              <div key={summary.factoryName} className="px-4 py-3">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="text-sm font-medium truncate">{summary.factoryName}</p>
                    <p className="text-xs text-muted-foreground">
                      {summary.totalTriggers} triggers · {summary.successRate.toFixed(0)}% create success · {summary.prMergeRate.toFixed(0)}% PR merge
                    </p>
                  </div>
                  <div className="shrink-0 text-right text-xs text-muted-foreground">
                    {summary.errors} errors
                  </div>
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  )
}

function AnalyticsMetric({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-lg border border-border p-3">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="text-xl font-semibold mt-1">{value}</p>
    </div>
  )
}
