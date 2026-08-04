// Persisted workspace selection for the nav rail's picker. This is UI state
// (like elasticclaw_selected_claw), never auth state: it only remembers which
// workspace the admin last picked so the choice survives navigating between
// screens that carry no workspace in the URL.
const SELECTED_WORKSPACE_KEY = "elasticclaw_selected_workspace"

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
}
