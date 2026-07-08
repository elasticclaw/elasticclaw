"use client"

import React, { useCallback, useEffect, useState } from "react"
import Link from "next/link"
import { AlertTriangle, ArrowRight, CheckCircle2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"

// linkifyText converts URLs in text into clickable <a> elements.
function linkifyText(text: string): React.ReactNode {
  const urlRegex = /(https?:\/\/[^\s]+)/g
  const parts = text.split(urlRegex)
  return parts.map((part, i) => {
    if (part.match(urlRegex)) {
      return (
        <a
          key={i}
          href={part}
          target="_blank"
          rel="noopener noreferrer"
          className="text-primary hover:underline"
        >
          {part}
        </a>
      )
    }
    return part
  })
}

export default function DoctorSection() {
  const [report, setReport] = useState<any>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showPassed, setShowPassed] = useState(false)

  const load = useCallback(async (refresh = false) => {
    setLoading(true)
    setError(null)
    try {
      const hubUrl = getHubUrl()
      const token = getAuthToken() || ""
      const res = await fetch(`${hubUrl}/api/doctor${refresh ? "?refresh=true" : ""}`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!res.ok) throw new Error(await res.text())
      setReport(await res.json())
    } catch (e: any) {
      setError(e.message || "Failed to load diagnostics")
    }
    setLoading(false)
  }, [])

  useEffect(() => { load() }, [load])

  const failedChecks = report?.checks?.filter((c: any) => !c.ok) || []
  const passedChecks = report?.checks?.filter((c: any) => c.ok) || []
  const visibleChecks = showPassed ? report?.checks || [] : failedChecks
  const allPassing = failedChecks.length === 0 && report?.checks?.length > 0

  const severityIcon = (s: string) => {
    switch (s) {
      case "critical": return <AlertTriangle className="size-4 text-red-500" />
      case "warning": return <AlertTriangle className="size-4 text-amber-500" />
      default: return <CheckCircle2 className="size-4 text-blue-400" />
    }
  }

  const severityBadge = (s: string) => {
    const classes: Record<string, string> = {
      critical: "bg-red-500/10 text-red-500 border-red-500/20",
      warning: "bg-amber-500/10 text-amber-500 border-amber-500/20",
      info: "bg-blue-400/10 text-blue-400 border-blue-400/20",
    }
    return (
      <span className={cn("text-[10px] uppercase tracking-wider font-semibold px-2 py-0.5 rounded border", classes[s] || classes.info)}>
        {s}
      </span>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold mb-1">Doctor</h2>
          <p className="text-sm text-muted-foreground">
            Diagnose hub configuration issues and get actionable fixes.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {report?.cachedAt && (
            <span className="text-xs text-muted-foreground">
              Cached {new Date(report.cachedAt).toLocaleTimeString()}
            </span>
          )}
          {report && passedChecks.length > 0 && (
            <Button
              size="sm"
              variant={showPassed ? "default" : "outline"}
              onClick={() => setShowPassed(!showPassed)}
            >
              <CheckCircle2 className="size-3.5 mr-1.5" />
              {showPassed ? "Hide passed" : `Show passed (${passedChecks.length})`}
            </Button>
          )}
        </div>
      </div>

      {error && (
        <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-4 text-sm text-red-500">
          {error}
        </div>
      )}

      {report && (
        <>
          <div className="grid grid-cols-4 gap-3">
            <div className="rounded-lg border border-border p-3 text-center">
              <p className="text-2xl font-bold">{report.summary.total}</p>
              <p className="text-xs text-muted-foreground mt-1">Checks</p>
            </div>
            <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-red-500">{report.summary.critical}</p>
              <p className="text-xs text-muted-foreground mt-1">Critical</p>
            </div>
            <div className="rounded-lg border border-amber-500/20 bg-amber-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-amber-500">{report.summary.warning}</p>
              <p className="text-xs text-muted-foreground mt-1">Warnings</p>
            </div>
            <div className="rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-3 text-center">
              <p className="text-2xl font-bold text-emerald-500">{report.summary.passed}</p>
              <p className="text-xs text-muted-foreground mt-1">Passed</p>
            </div>
          </div>

          {visibleChecks.length === 0 ? (
            allPassing ? (
              <div className="rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-6 text-center">
                <CheckCircle2 className="size-8 text-emerald-500 mx-auto mb-2" />
                <p className="text-sm font-medium text-emerald-500">All checks passing</p>
                <p className="text-xs text-muted-foreground mt-1">
                  {report.summary.passed} check{report.summary.passed !== 1 ? "s" : ""} passed with no issues
                </p>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">No checks to display.</p>
            )
          ) : (
            <div className="space-y-3">
              {visibleChecks.map((check: any, i: number) => (
                <div
                  key={i}
                  className={cn(
                    "rounded-lg border p-4",
                    check.ok
                      ? "border-emerald-500/20 bg-emerald-500/5"
                      : check.severity === "critical"
                        ? "border-red-500/20 bg-red-500/5"
                        : check.severity === "warning"
                          ? "border-amber-500/20 bg-amber-500/5"
                          : "border-border"
                  )}
                >
                  <div className="flex items-start gap-3">
                    <div className="mt-0.5 shrink-0">
                      {check.ok ? (
                        <CheckCircle2 className="size-4 text-emerald-500" />
                      ) : (
                        severityIcon(check.severity)
                      )}
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
                          {check.category}
                        </span>
                        {!check.ok && severityBadge(check.severity)}
                      </div>
                      <p className="text-sm font-medium mt-1">{check.title}</p>
                      <p className="text-sm text-muted-foreground mt-0.5 whitespace-pre-wrap">{linkifyText(check.description)}</p>
                      {check.error && (
                        <p className="text-xs text-red-400 mt-1 font-mono">{check.error}</p>
                      )}
                      {check.fixAction && !check.ok && (
                        <div className="mt-3">
                          {check.fixAction.type === "navigate" ? (
                            <Button
                              size="sm"
                              variant="outline"
                              className="text-xs"
                              asChild
                            >
                              <Link href={check.fixAction.target}>
                                {check.fixAction.label}
                                <ArrowRight className="size-3 ml-1" />
                              </Link>
                            </Button>
                          ) : (
                            <Button
                              size="sm"
                              variant="outline"
                              className="text-xs"
                              disabled
                              title={`Action type "${check.fixAction.type}" not yet supported in UI`}
                            >
                              {check.fixAction.label}
                            </Button>
                          )}
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}
