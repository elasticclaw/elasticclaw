"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { RotateCcw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"

const TIME_RANGES = [
  { id: "just-now", label: "Just now", desc: "last 5 min" },
  { id: "15min", label: "Last 15 min", desc: "last 15 min" },
  { id: "1hour", label: "Past hour", desc: "last 1 hour" },
  { id: "4hours", label: "~4 hours ago", desc: "last 4 hours" },
  { id: "today", label: "Today", desc: "since midnight" },
]

export default function TroubleshootSection() {
  const [problem, setProblem] = useState("")
  const [timeRange, setTimeRange] = useState<string | null>(null)
  const [stillHappening, setStillHappening] = useState<boolean | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [diagnosis, setDiagnosis] = useState<string | null>(null)
  const [submitted, setSubmitted] = useState(false)
  const [streamingText, setStreamingText] = useState("")

  const typewriterQueueRef = useRef<string[]>([])
  const typewriterIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const contentRef = useRef<string>("")
  const diagnosisScrollRef = useRef<HTMLDivElement>(null)

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const canSubmit = problem.trim() !== "" && timeRange !== null && !loading

  const startTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) return
    typewriterIntervalRef.current = setInterval(() => {
      const queue = typewriterQueueRef.current
      if (queue.length === 0) return
      const chars = queue.splice(0, 3).join("")
      contentRef.current += chars
      setStreamingText(contentRef.current + "▌")
      if (diagnosisScrollRef.current) {
        diagnosisScrollRef.current.scrollTop = diagnosisScrollRef.current.scrollHeight
      }
    }, 20)
  }, [])

  const stopTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) {
      clearInterval(typewriterIntervalRef.current)
      typewriterIntervalRef.current = null
    }
  }, [])

  useEffect(() => () => { stopTypewriter() }, [stopTypewriter])

  const submit = async () => {
    if (!canSubmit) return
    setLoading(true)
    setError(null)
    setDiagnosis(null)
    setSubmitted(true)
    setStreamingText("")
    contentRef.current = ""
    typewriterQueueRef.current = []

    try {
      const body: Record<string, unknown> = { problem: problem.trim(), time_range: timeRange }
      if (stillHappening !== null) body.still_happening = stillHappening

      const res = await fetch(`${hubUrl}/api/troubleshoot/stream`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
      if (!res.ok) throw new Error(await res.text())

      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let sseBuffer = ""

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        sseBuffer += decoder.decode(value, { stream: true })
        const lines = sseBuffer.split("\n")
        sseBuffer = lines.pop() ?? ""
        for (const line of lines) {
          if (!line.startsWith("data: ")) continue
          let parsed: Record<string, unknown>
          try { parsed = JSON.parse(line.slice(6)) } catch { continue }
          if (parsed.type === "token") {
            typewriterQueueRef.current.push(...(parsed.content as string).split(""))
            startTypewriter()
          } else if (parsed.type === "error") {
            setError(parsed.content as string)
          }
        }
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed")
    } finally {
      stopTypewriter()
      const remaining = typewriterQueueRef.current.join("")
      typewriterQueueRef.current = []
      contentRef.current += remaining
      setStreamingText("")
      setDiagnosis(contentRef.current)
      contentRef.current = ""
      setLoading(false)
    }
  }

  const reset = () => {
    setProblem("")
    setTimeRange(null)
    setStillHappening(null)
    setLoading(false)
    setError(null)
    setDiagnosis(null)
    setSubmitted(false)
    setStreamingText("")
    stopTypewriter()
    typewriterQueueRef.current = []
    contentRef.current = ""
  }

  const displayText = diagnosis || streamingText

  return (
    <div className="flex flex-col" style={{ height: "calc(100vh - 8rem)" }}>
      <div className="px-8 pt-6 pb-3 flex-none flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold mb-0.5">Troubleshoot</h2>
          <p className="text-sm text-muted-foreground">
            Describe what&apos;s not working. The AI will analyze your logs, config, and source code.
          </p>
        </div>
        {submitted && (
          <Button size="sm" variant="outline" onClick={reset} disabled={loading} className="shrink-0 mt-0.5">
            Start over
          </Button>
        )}
      </div>

      {!submitted ? (
        <div className="flex-1 overflow-y-auto px-8 pb-8">
          <div className="space-y-5 max-w-lg">
            <div>
              <label className="text-sm font-medium mb-2 block">What&apos;s happening?</label>
              <textarea
                className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring min-h-[100px] resize-none"
                placeholder="e.g. Workflow webhook is firing but the machine isn't being bootstrapped..."
                value={problem}
                onChange={e => setProblem(e.target.value)}
                disabled={loading}
              />
            </div>

            <div>
              <label className="text-sm font-medium mb-2 block">When did this happen?</label>
              <div className="flex flex-wrap gap-2">
                {TIME_RANGES.map(({ id, label }) => (
                  <Button
                    key={id}
                    size="sm"
                    variant={timeRange === id ? "default" : "outline"}
                    onClick={() => setTimeRange(id)}
                    disabled={loading}
                  >
                    {label}
                  </Button>
                ))}
              </div>
            </div>

            <div>
              <label className="text-sm font-medium mb-2 block">Still happening?</label>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant={stillHappening === true ? "default" : "outline"}
                  onClick={() => setStillHappening(stillHappening === true ? null : true)}
                  disabled={loading}
                >
                  Yes
                </Button>
                <Button
                  size="sm"
                  variant={stillHappening === false ? "default" : "outline"}
                  onClick={() => setStillHappening(stillHappening === false ? null : false)}
                  disabled={loading}
                >
                  No
                </Button>
              </div>
            </div>

            <Button onClick={submit} disabled={!canSubmit}>
              Diagnose
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex-1 min-h-0 flex flex-col px-8 pb-8 gap-4 overflow-hidden">
          <div className="rounded-lg border border-border bg-secondary/30 p-4 flex-none">
            <p className="text-xs text-muted-foreground mb-1">Your report</p>
            <p className="text-sm">{problem.trim()}</p>
            <div className="flex gap-3 mt-2">
              <span className="text-xs text-muted-foreground">{TIME_RANGES.find(t => t.id === timeRange)?.label}</span>
              {stillHappening !== null && (
                <span className="text-xs text-muted-foreground">• {stillHappening ? "Still happening" : "Resolved"}</span>
              )}
            </div>
          </div>

          {error && (
            <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-3 text-sm text-red-500 flex-none">
              {error}
            </div>
          )}

          {loading && !streamingText && (
            <div className="flex items-center gap-2 text-sm text-muted-foreground flex-none">
              <RotateCcw className="size-3.5 animate-spin" />
              Gathering logs and analyzing…
            </div>
          )}

          {displayText && (
            <div
              ref={diagnosisScrollRef}
              className="flex-1 overflow-y-auto rounded-lg border border-border p-4 text-sm whitespace-pre-wrap font-mono leading-relaxed bg-background"
            >
              {displayText}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
