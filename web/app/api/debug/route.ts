import { NextResponse } from "next/server"

// Debug endpoint — shows config status without exposing secrets
// Only accessible to authenticated sessions (enforced by middleware)
export async function GET() {
  const hubUrl = process.env.ELASTICCLAW_HUB_URL || process.env.HUB_URL || process.env.NEXT_PUBLIC_HUB_URL || "http://localhost:18788"
  const token = process.env.ELASTICCLAW_HUB_TOKEN || process.env.HUB_TOKEN || process.env.NEXT_PUBLIC_HUB_TOKEN || ""
  const uiToken = process.env.ELASTICCLAW_UI_TOKEN

  // Try to reach the hub
  let hubReachable = false
  let hubError: string | null = null
  try {
    const res = await fetch(`${hubUrl}/api/claws`, {
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(3000),
    })
    hubReachable = res.ok
    if (!res.ok) hubError = `HTTP ${res.status}`
  } catch (e) {
    hubError = e instanceof Error ? e.message : String(e)
  }

  return NextResponse.json({
    hub: {
      url: hubUrl,
      tokenSet: !!token,
      tokenPrefix: token ? token.slice(0, 6) + "..." : null,
      reachable: hubReachable,
      error: hubError,
    },
    ui: {
      tokenSet: !!uiToken,
    },
    env: process.env.NODE_ENV,
  })
}
