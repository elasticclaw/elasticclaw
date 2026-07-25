"use client"

import { useEffect, useState } from "react"
import { ExternalLink, Loader2 } from "lucide-react"
import Link from "next/link"
import { fetchClaw } from "@/lib/api"
import { Button } from "@/components/ui/button"

type WaitingState = "starting" | "waiting" | "failed"

export default function PreviewWaitingPage() {
  const [state, setState] = useState<WaitingState>("starting")
  const [message, setMessage] = useState("Starting the ElasticClaw task…")

  useEffect(() => {
    const clawIDParam = new URLSearchParams(window.location.search).get("claw")
    if (!clawIDParam) return
    const clawID = clawIDParam

    let cancelled = false
    let timer: number | undefined

    async function poll() {
      try {
        const claw = await fetchClaw(clawID)
        if (cancelled) return
        if (claw.preview_ready && claw.preview_url) {
          const target = new URL(claw.preview_url)
          if (target.protocol !== "http:" && target.protocol !== "https:") {
            throw new Error("ElasticClaw returned an unsupported preview URL.")
          }
          window.opener = null
          window.location.replace(target.toString())
          return
        }
        if (claw.status === "error") {
          setState("failed")
          setMessage("The agent stopped before its browser preview became ready.")
          return
        }
        setState("waiting")
        setMessage(
          claw.preview_port
            ? `Waiting for the agent to verify the changed page on port ${claw.preview_port}…`
            : "Waiting for the browser preview to become available…"
        )
        timer = window.setTimeout(poll, 2000)
      } catch (error) {
        if (cancelled) return
        setState("failed")
        setMessage(error instanceof Error ? error.message : "Could not load the preview status.")
      }
    }

    void poll()
    return () => {
      cancelled = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [])

  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-6">
      <section className="w-full max-w-md rounded-lg border border-border bg-card p-8 text-center shadow-sm">
        {state === "failed" ? (
          <ExternalLink className="mx-auto mb-4 size-8 text-red-500" />
        ) : (
          <Loader2 className="mx-auto mb-4 size-8 animate-spin text-emerald-500" />
        )}
        <h1 className="text-lg font-semibold text-foreground">
          {state === "failed" ? "Preview unavailable" : "Preparing browser preview"}
        </h1>
        <p className="mt-2 text-sm text-muted-foreground">{message}</p>
        <Button asChild variant="outline" className="mt-6">
          <Link href="/">Return to ElasticClaw</Link>
        </Button>
      </section>
    </main>
  )
}
