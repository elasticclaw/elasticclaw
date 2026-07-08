"use client"

import { useEffect, useRef, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"

export interface ModelAuthLoginJob {
  id: string
  provider: string
  profile: string
  status: string
  url?: string
  code?: string
  output?: string
  error?: string
}

async function startModelAuthLogin(provider: string, profile: string): Promise<ModelAuthLoginJob> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings/model-auth/login`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify({ provider, profile, mode: "device" }),
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

async function fetchModelAuthLogin(id: string): Promise<ModelAuthLoginJob> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings/model-auth/login/${encodeURIComponent(id)}`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

function renderLoginWindow(win: Window, text: string) {
  win.document.body.style.margin = "0"
  win.document.body.style.background = "#0a0a0a"
  win.document.body.style.color = "#f5f5f5"
  win.document.body.style.fontFamily = "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace"
  win.document.body.style.whiteSpace = "pre-wrap"
  win.document.body.style.padding = "24px"
  win.document.body.textContent = text
}

/**
 * Device-flow CLI login for model providers (codex/grok): starts a login job,
 * polls its status, and mirrors progress into a popup window.
 */
export function useModelAuthLogin() {
  const [loginJob, setLoginJob] = useState<ModelAuthLoginJob | null>(null)
  const [loginError, setLoginError] = useState("")
  const [copiedLoginCode, setCopiedLoginCode] = useState(false)
  const loginWindowRef = useRef<Window | null>(null)

  function copyLoginCode(code: string) {
    navigator.clipboard.writeText(code).then(() => {
      setCopiedLoginCode(true)
      setTimeout(() => setCopiedLoginCode(false), 2000)
    })
  }

  async function startLogin(provider: string, profile: string) {
    setLoginError("")
    loginWindowRef.current = window.open("", "_blank")
    if (loginWindowRef.current) {
      loginWindowRef.current.document.title = "Model login"
      renderLoginWindow(loginWindowRef.current, "Waiting for login URL...")
    }
    try {
      const job = await startModelAuthLogin(provider, profile)
      setLoginJob(job)
      if (job.url && loginWindowRef.current && !loginWindowRef.current.closed) {
        loginWindowRef.current.location.href = job.url
        loginWindowRef.current = null
      }
    } catch (err) {
      if (loginWindowRef.current && !loginWindowRef.current.closed) {
        loginWindowRef.current.close()
      }
      loginWindowRef.current = null
      setLoginError(err instanceof Error ? err.message : String(err))
    }
  }

  function resetLogin() {
    setLoginJob(null)
    setLoginError("")
    setCopiedLoginCode(false)
  }

  useEffect(() => {
    if (!loginJob || loginJob.status !== "running") return
    const timer = window.setInterval(async () => {
      try {
        const next = await fetchModelAuthLogin(loginJob.id)
        setLoginJob(next)
      } catch (err) {
        setLoginError(err instanceof Error ? err.message : String(err))
      }
    }, 1500)
    return () => window.clearInterval(timer)
  }, [loginJob])

  useEffect(() => {
    if (!loginJob?.url || !loginWindowRef.current || loginWindowRef.current.closed) return
    loginWindowRef.current.location.href = loginJob.url
    loginWindowRef.current = null
  }, [loginJob?.url])

  useEffect(() => {
    if (!loginJob || !loginWindowRef.current || loginWindowRef.current.closed || loginJob.url) return
    const lines = [`Login status: ${loginJob.status}`]
    if (loginJob.error) lines.push("", loginJob.error)
    if (loginJob.output) lines.push("", loginJob.output)
    renderLoginWindow(loginWindowRef.current, lines.join("\n"))
  }, [loginJob])

  return { loginJob, loginError, copiedLoginCode, startLogin, copyLoginCode, resetLogin }
}
