"use client"

import { useCallback, useSyncExternalStore } from "react"

// Per-claw "when did we last hear anything" store. Event handlers (websocket
// chunk/activity/message arrivals) call noteOutput; the Now strip subscribes
// via useLastOutputAt. Kept outside React state so a chunk flood does not
// force extra renders — subscribers only re-read on notification.

interface OutputEntry {
  at: number | null
  listeners: Set<() => void>
}

const entries = new Map<string, OutputEntry>()

function entryFor(clawId: string): OutputEntry {
  let entry = entries.get(clawId)
  if (!entry) {
    entry = { at: null, listeners: new Set() }
    entries.set(clawId, entry)
  }
  return entry
}

/** Record that output arrived for a claw. Call from event handlers only. */
export function noteOutput(clawId: string): void {
  const entry = entryFor(clawId)
  entry.at = Date.now()
  entry.listeners.forEach((notify) => notify())
}

/** Timestamp (ms) of the last observed output for a claw, or null. */
export function useLastOutputAt(clawId: string): number | null {
  const subscribe = useCallback(
    (listener: () => void) => {
      const entry = entryFor(clawId)
      entry.listeners.add(listener)
      return () => entry.listeners.delete(listener)
    },
    [clawId]
  )
  const getSnapshot = useCallback(() => entryFor(clawId).at, [clawId])
  return useSyncExternalStore(subscribe, getSnapshot, () => null)
}
