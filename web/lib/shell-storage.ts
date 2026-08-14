// localStorage-backed shell state, exposed as an external store.
//
// Reading storage inside a useState initializer makes the first client render
// disagree with the SSR pass and React throws the whole tree away. useSync-
// ExternalStore is the sanctioned way out: it hydrates against the server
// snapshot and then re-renders with the client one, with no effect and no
// cascading setState.

import { isConfigured } from "@/lib/api"

const SELECTED_CLAW_KEY = "elasticclaw_selected_claw"

const listeners = new Set<() => void>()

function emit() {
  for (const listener of listeners) listener()
}

export function subscribeToShellStorage(listener: () => void) {
  listeners.add(listener)
  // Another tab writing the same keys should move this one too.
  const onStorage = (event: StorageEvent) => {
    if (event.key === null || event.key === SELECTED_CLAW_KEY) listener()
  }
  window.addEventListener("storage", onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener("storage", onStorage)
  }
}

export function getSelectedClawId(): string | null {
  return localStorage.getItem(SELECTED_CLAW_KEY)
}

export function getServerSelectedClawId(): string | null {
  return null
}

export function writeSelectedClawId(clawId: string | null) {
  if (clawId) localStorage.setItem(SELECTED_CLAW_KEY, clawId)
  else localStorage.removeItem(SELECTED_CLAW_KEY)
  emit()
}

export function getConfigured(): boolean {
  return isConfigured()
}

/** null means "not known yet" — the shell renders its placeholder. */
export function getServerConfigured(): boolean | null {
  return null
}

/** Lets the setup screen flip the shell over once the hub is reachable. */
export function markConfigured() {
  emit()
}
