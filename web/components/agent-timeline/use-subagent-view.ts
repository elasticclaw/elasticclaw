"use client"

import { useSyncExternalStore } from "react"

/** Where subagents are shown: the right rail, the top lane strip, or nowhere. */
export type SubagentView = "rail" | "lanes" | "off"

const VIEW_STORAGE_KEY = "elasticclaw.subagents.view"
const DEFAULT_VIEW: SubagentView = "rail"

const VIEW_VALUES: SubagentView[] = ["rail", "lanes", "off"]

function isView(value: string | null): value is SubagentView {
  return VIEW_VALUES.includes(value as SubagentView)
}

// Same tiny external store as useTimelineDensity: every open chat shares the
// persisted choice, and the server snapshot is the default so
// useSyncExternalStore reconciles the stored value after hydration instead of
// mismatching during it.
let viewCache: SubagentView | null = null
const viewListeners = new Set<() => void>()

function readView(): SubagentView {
  if (viewCache === null) {
    try {
      const stored = localStorage.getItem(VIEW_STORAGE_KEY)
      viewCache = isView(stored) ? stored : DEFAULT_VIEW
    } catch {
      viewCache = DEFAULT_VIEW
    }
  }
  return viewCache
}

function writeView(view: SubagentView): void {
  viewCache = view
  try {
    localStorage.setItem(VIEW_STORAGE_KEY, view)
  } catch {}
  viewListeners.forEach((notify) => notify())
}

function subscribeView(listener: () => void): () => void {
  viewListeners.add(listener)
  return () => viewListeners.delete(listener)
}

/** Persisted subagent view choice (localStorage-backed, shared across chats). */
export function useSubagentView(): [SubagentView, (v: SubagentView) => void] {
  const view = useSyncExternalStore(subscribeView, readView, () => DEFAULT_VIEW)
  return [view, writeView]
}
