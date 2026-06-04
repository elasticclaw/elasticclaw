import type { Page } from "@playwright/test"

interface SessionOptions {
  hubToken?: string
  githubToken?: string
}

export async function installSession(page: Page, options: SessionOptions = {}) {
  const hubToken = options.hubToken ?? "mock-hub-token"
  const githubToken = options.githubToken ?? null

  await page.addInitScript(
    ({ hubToken, githubToken }) => {
      sessionStorage.setItem("ec_hub_token", hubToken)
      if (githubToken) sessionStorage.setItem("ec_github_token", githubToken)

      localStorage.removeItem("elasticclaw_selected_claw")
      localStorage.removeItem("elasticclaw_messages")
      localStorage.removeItem("elasticclaw_pinned")
      localStorage.removeItem("elasticclaw_claw_order")
    },
    { hubToken, githubToken }
  )
}
