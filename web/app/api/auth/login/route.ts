import { NextRequest, NextResponse } from "next/server"

// Dev-mode proxy: forwards login to the hub and explicitly copies Set-Cookie back
// to the browser's origin (Next.js port), so the session cookie works cross-port.
// In production (embedded), the hub serves this directly on the same port.
export async function POST(req: NextRequest) {
  const hubUrl =
    process.env.ELASTICCLAW_HUB_URL ||
    process.env.HUB_URL ||
    "http://localhost:8080"

  const body = await req.text()

  const upstream = await fetch(`${hubUrl}/api/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  })

  const data = await upstream.text()
  const res = new NextResponse(data, { status: upstream.status })

  // Forward Set-Cookie from hub so browser stores it on the Next.js port
  const setCookie = upstream.headers.get("set-cookie")
  if (setCookie) {
    res.headers.set("set-cookie", setCookie)
  }

  return res
}
