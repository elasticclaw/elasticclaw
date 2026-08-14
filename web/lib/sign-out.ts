import { clearConfig } from "@/lib/api"
import { getAuthToken } from "@/lib/auth-storage"

/** Best-effort server logout, then clear local config and go to /login. */
export async function signOut(): Promise<void> {
  const token = getAuthToken() || ""
  if (token) {
    const { getHubUrl } = await import("@/lib/hub-url")
    const hubUrl = getHubUrl()
    const logoutUrl = hubUrl ? `${hubUrl}/api/auth/logout` : "/api/auth/logout"
    await fetch(logoutUrl, { method: "POST", headers: { Authorization: `Bearer ${token}` } }).catch(() => {})
  }
  clearConfig()
  window.location.href = "/login"
}
