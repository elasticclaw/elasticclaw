"use client"

import { useRef, useCallback, useEffect, useState } from "react"

const CHARS_PER_MS = 0.18 // ~180 chars/sec — fast enough to feel live, slow enough to read

interface TypewriterEntry {
  clawId: string
  queue: string    // text waiting to be revealed
  shown: string    // text already revealed
  done: boolean    // final message received — drain then stop
  hadChunks: boolean // true once any chunk has arrived
  pausedAt: number | null // timestamp when queue last emptied mid-stream
}

/**
 * useTypewriter
 *
 * Takes raw streaming buffers (chunks as they arrive from WS) and produces
 * smoothly-drained display buffers that feel like a typewriter.
 *
 * Usage:
 *   const { displayBuffers, pushChunk, finalize, split, clear } = useTypewriter()
 *
 *   pushChunk(clawId, chunkText)   — call on every WS chunk event
 *   finalize(clawId)               — call when the final message arrives
 *   displayBuffers[clawId]         — current visible text (or undefined when done)
 */
export interface TypewriterState {
  text: string       // currently visible text
  hadChunks: boolean // at least one chunk received
  isPaused: boolean  // had chunks, queue empty, not done (tool call gap)
}

export function useTypewriter() {
  const entries = useRef<Record<string, TypewriterEntry>>({})
  const rafRef = useRef<number | null>(null)
  const lastTickRef = useRef<number>(0)
  const [displayBuffers, setDisplayBuffers] = useState<Record<string, TypewriterState>>({})

  const tick = useCallback((now: number) => {
    const dt = lastTickRef.current ? now - lastTickRef.current : 16
    lastTickRef.current = now

    const charsToReveal = Math.max(1, Math.round(CHARS_PER_MS * dt))
    let anyActive = false
    let changed = false

    for (const entry of Object.values(entries.current)) {
      if (entry.queue.length === 0) {
        if (entry.done) {
          // All drained — fire callback then remove
          const cb = onDrainCallbacks.current[entry.clawId]
          if (cb) {
            delete onDrainCallbacks.current[entry.clawId]
            cb()
          }
          delete entries.current[entry.clawId]
          changed = true
        } else if (entry.hadChunks && entry.pausedAt === null) {
          // Queue emptied mid-stream — note when it happened
          entry.pausedAt = now
          // Don't set changed yet — we wait 500ms before showing the gap
        } else if (entry.hadChunks && entry.pausedAt !== null && now - entry.pausedAt >= 500) {
          // Pause has lasted 500ms — now surface it
          changed = true
        }
        continue
      }

      // New chunks arrived — clear pause
      if (entry.pausedAt !== null) {
        entry.pausedAt = null
        changed = true
      }

      anyActive = true
      const reveal = entry.queue.slice(0, charsToReveal)
      entry.queue = entry.queue.slice(charsToReveal)
      entry.shown += reveal
      entry.hadChunks = true
      changed = true
    }

    if (changed) {
      const snapshot: Record<string, TypewriterState> = {}
      for (const [id, e] of Object.entries(entries.current)) {
        snapshot[id] = {
          text: e.shown,
          hadChunks: e.hadChunks,
          isPaused: e.hadChunks && e.queue.length === 0 && !e.done && e.pausedAt !== null,
        }
      }
      setDisplayBuffers(snapshot)
    }

    // Keep running if any entry is paused mid-stream (waiting for 500ms threshold)
    const anyPending = Object.values(entries.current).some(
      (e) => e.queue.length > 0 || (e.hadChunks && !e.done && e.pausedAt !== null)
    )
    if (anyActive || anyPending) {
      rafRef.current = requestAnimationFrame(tick)
    } else {
      rafRef.current = null
    }
  }, [])

  const ensureRunning = useCallback(() => {
    if (!rafRef.current) {
      lastTickRef.current = 0
      rafRef.current = requestAnimationFrame(tick)
    }
  }, [tick])

  const pushChunk = useCallback((clawId: string, chunk: string) => {
    if (!entries.current[clawId]) {
      entries.current[clawId] = { clawId, queue: "", shown: "", done: false, hadChunks: false, pausedAt: null }
      // Trigger a render so the thinking indicator appears immediately
      setDisplayBuffers((prev) => ({ ...prev, [clawId]: { text: "", hadChunks: false, isPaused: false } }))
    }
    if (chunk) {
      entries.current[clawId].queue += chunk
    }
    ensureRunning()
  }, [ensureRunning])

  // onDrain callbacks: fired once when entry fully drains after finalize()
  const onDrainCallbacks = useRef<Record<string, () => void>>({})

  /** Call when the final complete message arrives from WS.
   *  onDrain is called once the typewriter has fully drained. */
  const finalize = useCallback((clawId: string, onDrain?: () => void) => {
    if (entries.current[clawId]) {
      entries.current[clawId].done = true
      if (onDrain) onDrainCallbacks.current[clawId] = onDrain
    } else {
      // No active entry — fire immediately
      onDrain?.()
    }
  }, [])

  /** Snapshot and reset the currently displayed stream at an activity boundary. */
  const split = useCallback((clawId: string): string => {
    const entry = entries.current[clawId]
    if (!entry) return ""
    const text = entry.shown + entry.queue
    entry.shown = ""
    entry.queue = ""
    entry.hadChunks = false
    entry.pausedAt = null
    entry.done = false
    setDisplayBuffers((prev) => ({ ...prev, [clawId]: { text: "", hadChunks: false, isPaused: false } }))
    return text
  }, [])

  /** Snapshot and remove the currently displayed stream. */
  const clear = useCallback((clawId: string): string => {
    const entry = entries.current[clawId]
    if (!entry) return ""
    const text = entry.shown + entry.queue
    delete entries.current[clawId]
    delete onDrainCallbacks.current[clawId]
    setDisplayBuffers((prev) => {
      const next = { ...prev }
      delete next[clawId]
      return next
    })
    return text
  }, [])

  /** Check if a claw is still draining */
  const isTyping = useCallback((clawId: string) => {
    return Boolean(entries.current[clawId])
  }, [])

  useEffect(() => {
    return () => {
      if (rafRef.current) cancelAnimationFrame(rafRef.current)
    }
  }, [])

  return { displayBuffers, pushChunk, finalize, split, clear, isTyping }
}
