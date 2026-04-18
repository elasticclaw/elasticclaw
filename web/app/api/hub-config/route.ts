import { NextRequest, NextResponse } from "next/server"
import { cookies } from "next/headers"

// Dev-mode proxy: fetches hub-config from the hub using the session cookie,
// then returns it to the browser. This avoids cross-port cookie issues.
// In production (embedded), the hub serves this directly.
export async function GET(req: NextRequest) {
  const hubUrl =
    process.env.ELASTICCLAW_HUB_URL ||
    process.env.HUB_URL ||
    "http://localhost:8080"

  const cookieStore = await cookies()
  const session = cookieStore.get("elasticclaw_session")

  const upstream = await fetch(`${hubUrl}/api/hub-config`, {
    headers: session ? { Cookie: `elasticclaw_session=${session.value}` } : {},
  })

  if (!upstream.ok) {
    return NextResponse.json({ token: "" }, { status: upstream.status })
  }

  const data = await upstream.json()
  return NextResponse.json(data)
}
