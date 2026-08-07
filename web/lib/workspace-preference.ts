// Persisted workspace selection for the nav rail's picker. This is UI state
// (like elasticclaw_selected_claw), never auth state: it only remembers which
// workspace the admin last picked so the choice survives navigating between
// screens that carry no workspace in the URL.
const SELECTED_WORKSPACE_KEY = "elasticclaw_selected_workspace"

// Settings URLs no longer carry the workspace during in-app navigation, so a
// picker change cannot rely on a route change to reach the settings screen.
// setStoredWorkspace announces the new selection on this event instead.
const WORKSPACE_CHANGE_EVENT = "elasticclaw:workspace-change"

export function getStoredWorkspace(): string {
  if (typeof window === "undefined") return ""
  try {
    return localStorage.getItem(SELECTED_WORKSPACE_KEY) ?? ""
  } catch {
    return ""
  }
}

export function setStoredWorkspace(name: string) {
  try {
    localStorage.setItem(SELECTED_WORKSPACE_KEY, name)
  } catch {
    // Storage can be unavailable (private mode, quota); the picker still
    // works for the current screen, the choice just won't persist.
  }
  window.dispatchEvent(new CustomEvent<string>(WORKSPACE_CHANGE_EVENT, { detail: name }))
}

// Subscribes to selection changes made through setStoredWorkspace in this
// tab. Returns the unsubscribe function.
export function onStoredWorkspaceChange(handler: (name: string) => void): () => void {
  const listener = (event: Event) => handler((event as CustomEvent<string>).detail)
  window.addEventListener(WORKSPACE_CHANGE_EVENT, listener)
  return () => window.removeEventListener(WORKSPACE_CHANGE_EVENT, listener)
}
