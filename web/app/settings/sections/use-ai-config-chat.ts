"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import { extractYamlPlaceholders, normalizeStoredMessage, stripYamlBlocks, type ChatMessage } from "./ai-config-markdown"

// Session storage keys
const SS_CHAT_KEY = "ai-config-chat-history"
const SS_YAML_KEY = "ai-config-proposed-yaml"
const SS_BACKUP_KEY = "ai-config-backup-path"

/**
 * Owns the "Configure with AI" chat: SSE streaming with a typewriter effect,
 * YAML proposal extraction, sessionStorage persistence, and apply/revert of
 * the proposed hub.yaml.
 */
export function useAIConfigChat() {
  // Load persisted state from sessionStorage — always start empty to avoid SSR hydration mismatch,
  // then restore from sessionStorage after mount.
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState("")
  const [loading, setLoading] = useState(false)
  const [proposedYaml, setProposedYaml] = useState<string | null>(null)
  const [currentConfig, setCurrentConfig] = useState<string | null>(null)
  const [placeholders, setPlaceholders] = useState<string[]>([])
  const [secretValues, setSecretValues] = useState<Record<string, string>>({})
  const [backupPath, setBackupPath] = useState<string | null>(null)
  // Restore from sessionStorage after mount (client-only)
  useEffect(() => {
    try {
      const raw = sessionStorage.getItem(SS_CHAT_KEY)
      if (raw) setMessages((JSON.parse(raw) as ChatMessage[]).map(normalizeStoredMessage))
      const yaml = sessionStorage.getItem(SS_YAML_KEY)
      if (yaml) {
        setProposedYaml(yaml)
        setPlaceholders(extractYamlPlaceholders(yaml))
        setSecretValues({})
      }
      const backup = sessionStorage.getItem(SS_BACKUP_KEY)
      if (backup) setBackupPath(backup)
    } catch { /* ignore */ }
  }, [])
  const [applying, setApplying] = useState(false)
  const [reverting, setReverting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [yamlStreaming, setYamlStreaming] = useState(false)
  const [streamingYaml, setStreamingYaml] = useState<string>("")
  const [applySuccess, setApplySuccess] = useState(false)
  const [revealSecrets, setRevealSecrets] = useState(false)

  // Typewriter queue
  const typewriterQueueRef = useRef<string[]>([])
  const typewriterIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const assistantContentRef = useRef<string>("")

  const chatScrollRef = useRef<HTMLDivElement>(null)
  const chatInputRef = useRef<HTMLInputElement>(null)

  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  // Persist messages to sessionStorage on change
  useEffect(() => {
    try {
      const persisted = messages.map(normalizeStoredMessage)
      sessionStorage.setItem(SS_CHAT_KEY, JSON.stringify(persisted))
    } catch {}
  }, [messages])

  // Persist proposedYaml
  useEffect(() => {
    try {
      if (proposedYaml) sessionStorage.setItem(SS_YAML_KEY, proposedYaml)
      else sessionStorage.removeItem(SS_YAML_KEY)
    } catch {}
  }, [proposedYaml])

  // Persist backupPath
  useEffect(() => {
    try {
      if (backupPath) sessionStorage.setItem(SS_BACKUP_KEY, backupPath)
      else sessionStorage.removeItem(SS_BACKUP_KEY)
    } catch {}
  }, [backupPath])

  // Load current config on mount (and when revealSecrets changes)
  useEffect(() => {
    const t = token()
    const url = revealSecrets
      ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
      : `${hubUrl}/api/settings/ai-config/current-config`
    fetch(url, {
      headers: { Authorization: `Bearer ${t}` },
    })
      .then(r => {
        if (!r.ok) throw new Error(`Failed to fetch current config: ${r.status}`)
        return r.text()
      })
      .then(text => setCurrentConfig(text))
      .catch(() => {})
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revealSecrets])

  // Load existing backup on mount (only if not already persisted)
  useEffect(() => {
    if (backupPath) return // already have one from sessionStorage
    const t = token()
    fetch(`${hubUrl}/api/settings/ai-config/backup`, {
      headers: { Authorization: `Bearer ${t}` },
    })
      .then(r => r.json())
      .then(d => { if (d.backup_path) setBackupPath(d.backup_path) })
      .catch(() => {})
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Auto-scroll chat to bottom when messages update
  useEffect(() => {
    if (chatScrollRef.current) {
      chatScrollRef.current.scrollTop = chatScrollRef.current.scrollHeight
    }
  }, [messages])

  // Typewriter: drain queue at ~20ms intervals
  const startTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) return
    typewriterIntervalRef.current = setInterval(() => {
      const queue = typewriterQueueRef.current
      if (queue.length === 0) return
      // Drain up to a few chars per tick for smooth ~60fps feel
      const chars = queue.splice(0, 3).join("")
      assistantContentRef.current += chars
      const current = stripYamlBlocks(assistantContentRef.current)
      setMessages(prev => {
        const msgs = [...prev]
        const last = msgs[msgs.length - 1]
        if (last?.role === "assistant" && last.streaming) {
          msgs[msgs.length - 1] = { role: "assistant", content: current + "▌", streaming: true }
        }
        return msgs
      })
    }, 20)
  }, [])

  const stopTypewriter = useCallback(() => {
    if (typewriterIntervalRef.current) {
      clearInterval(typewriterIntervalRef.current)
      typewriterIntervalRef.current = null
    }
  }, [])

  useEffect(() => () => { stopTypewriter() }, [stopTypewriter])

  // Drain remaining queue and finalize
  const finalizeTypewriter = useCallback((dropIfEmpty = false) => {
    stopTypewriter()
    // Flush remaining queue synchronously
    const remaining = typewriterQueueRef.current.join("")
    typewriterQueueRef.current = []
    assistantContentRef.current += remaining
    const finalContent = assistantContentRef.current
    const visibleFinalContent = stripYamlBlocks(finalContent)
    setMessages(prev => {
      const msgs = [...prev]
      const last = msgs[msgs.length - 1]
      if (last?.role === "assistant" && last.streaming) {
        if (dropIfEmpty && visibleFinalContent.trim() === "") return msgs.slice(0, -1)
        msgs[msgs.length - 1] = { role: "assistant", content: finalContent, streaming: false }
      }
      return msgs
    })
    assistantContentRef.current = ""
  }, [stopTypewriter])

  const sendMessage = async () => {
    if (!input.trim() || loading) return
    const userMsg: ChatMessage = { role: "user", content: input.trim() }
    const historyForRequest = [...messages]

    // Reset typewriter state
    stopTypewriter()
    typewriterQueueRef.current = []
    assistantContentRef.current = ""

    setMessages(prev => [...prev, userMsg])
    setInput("")
    setLoading(true)
    setError(null)
    setProposedYaml(null)
    setPlaceholders([])
    setSecretValues({})
    setYamlStreaming(false)
    setStreamingYaml("")

    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/stream`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ message: userMsg.content, history: historyForRequest }),
      })
      if (!res.ok) throw new Error(await res.text())

      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let sseBuffer = ""

      // Add streaming placeholder
      setMessages(prev => [...prev, { role: "assistant", content: "", streaming: true }])
      let inYamlBlock = false
      let yamlFenceConsumed = false

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
            const tokenText = parsed.content as string
            // Detect start of yaml block — redirect subsequent tokens to YAML panel
            const fullSoFar = assistantContentRef.current + typewriterQueueRef.current.join("") + tokenText
            if (!inYamlBlock && /```ya?ml/i.test(fullSoFar)) {
              const queued = typewriterQueueRef.current.join("")
              if (queued) {
                typewriterQueueRef.current = []
                assistantContentRef.current += queued
              }
              inYamlBlock = true
              yamlFenceConsumed = false
              setYamlStreaming(true)
              setStreamingYaml("")
            }
            if (inYamlBlock) {
              // Stream to YAML panel — strip the opening fence (may be mid-token or split across tokens)
              let content = tokenText
              if (!yamlFenceConsumed) {
                const fenceMatch = content.match(/```ya?ml\n?/i)
                if (fenceMatch && fenceMatch.index !== undefined) {
                  // Fence is in this token — drop everything up to and including the fence
                  content = content.slice(fenceMatch.index + fenceMatch[0].length)
                  yamlFenceConsumed = true
                } else {
                  // Fence was the entire previous token — mark consumed and pass content through
                  yamlFenceConsumed = true
                }
              }
              // Strip closing fence if it appears at the end
              const hadClosingFence = /```\s*$/.test(content)
              content = content.replace(/```\s*$/, "")
              if (content) setStreamingYaml(prev => prev + content)
              if (hadClosingFence) inYamlBlock = false
              // Still push to assistant content for stripping
              assistantContentRef.current += tokenText
            } else {
              // Push to typewriter queue instead of directly to state
              const chars = tokenText.split("")
              typewriterQueueRef.current.push(...chars)
              startTypewriter()
            }
            continue
          } else if (parsed.type === "proposed_yaml") {
            // YAML was already streamed live via token events — just finalize
            const yaml = parsed.yaml as string
            setYamlStreaming(false)
            setProposedYaml(yaml)
            setStreamingYaml("")
            inYamlBlock = false
          } else if (parsed.type === "placeholders") {
            setPlaceholders(parsed.items as string[])
            setSecretValues({})
          } else if (parsed.type === "error") {
            setError(parsed.content as string)
            finalizeTypewriter(true)
          } else if (parsed.type === "done") {
            // finalizeTypewriter() called in finally
          }
        }
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed")
    } finally {
      finalizeTypewriter(true)
      setYamlStreaming(false)
      setStreamingYaml("")
      setLoading(false)
      setTimeout(() => chatInputRef.current?.focus(), 0)
    }
  }

  const applyConfig = async () => {
    if (!proposedYaml) return
    setApplying(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/apply`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ proposed_yaml: proposedYaml, secrets: secretValues }),
      })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setBackupPath(data.backup_path)
      const cfgUrl = revealSecrets
        ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
        : `${hubUrl}/api/settings/ai-config/current-config`
      fetch(cfgUrl, {
        headers: { Authorization: `Bearer ${token()}` },
      }).then(r => r.text()).then(setCurrentConfig).catch(() => {})
      setProposedYaml(null)
      setPlaceholders([])
      setSecretValues({})
      setApplySuccess(true)
      setTimeout(() => setApplySuccess(false), 5000)
    } catch (e) {
      setError(e instanceof Error ? e.message : "Apply failed")
    } finally {
      setApplying(false)
    }
  }

  const revertConfig = async () => {
    if (!backupPath) return
    setReverting(true)
    setError(null)
    try {
      const res = await fetch(`${hubUrl}/api/settings/ai-config/revert`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
        body: JSON.stringify({ backup_path: backupPath }),
      })
      if (!res.ok) throw new Error(await res.text())
      setBackupPath(null)
      setApplySuccess(false)
      const cfgUrl = revealSecrets
        ? `${hubUrl}/api/settings/ai-config/current-config?reveal=true`
        : `${hubUrl}/api/settings/ai-config/current-config`
      fetch(cfgUrl, {
        headers: { Authorization: `Bearer ${token()}` },
      }).then(r => r.text()).then(setCurrentConfig).catch(() => {})
    } catch (e) {
      setError(e instanceof Error ? e.message : "Revert failed")
    } finally {
      setReverting(false)
    }
  }

  // "Start over": clear the chat and any persisted proposal/backup state.
  const startOver = () => {
    setMessages([])
    setProposedYaml(null)
    setPlaceholders([])
    setSecretValues({})
    setError(null)
    setApplySuccess(false)
    setYamlStreaming(false)
    setStreamingYaml("")
    sessionStorage.removeItem(SS_CHAT_KEY)
    sessionStorage.removeItem(SS_YAML_KEY)
    sessionStorage.removeItem(SS_BACKUP_KEY)
    setBackupPath(null)
    setTimeout(() => chatInputRef.current?.focus(), 0)
  }

  const discardProposed = () => {
    setProposedYaml(null)
    setPlaceholders([])
    setSecretValues({})
  }

  return {
    messages, input, setInput, loading, error,
    proposedYaml, currentConfig, placeholders, secretValues, setSecretValues,
    backupPath, applying, reverting, applySuccess,
    yamlStreaming, streamingYaml,
    revealSecrets, setRevealSecrets,
    chatScrollRef, chatInputRef,
    sendMessage, applyConfig, revertConfig, startOver, discardProposed,
  }
}
