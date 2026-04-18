import { NextResponse } from "next/server"

// Returns hub config to authenticated browser — session is already verified by middleware
export async function GET() {
  const token = process.env.ELASTICCLAW_HUB_TOKEN || process.env.HUB_TOKEN || process.env.NEXT_PUBLIC_HUB_TOKEN || ""
  const hubUrl = process.env.ELASTICCLAW_HUB_URL || process.env.HUB_URL || process.env.NEXT_PUBLIC_HUB_URL || "http://localhost:18788"

  const debug = {
    token: token ? `${token.slice(0, 6)}...` : "(not set)",
    hubUrl,
    hasToken: !!token,
    hasHubUrl: !!process.env.ELASTICCLAW_HUB_URL,
  }

  if (!token) console.warn("[elasticclaw] ELASTICCLAW_HUB_TOKEN is not set")
  if (!process.env.ELASTICCLAW_HUB_URL) console.warn("[elasticclaw] ELASTICCLAW_HUB_URL is not set — defaulting to", hubUrl)

  return NextResponse.json({ token, hubUrl, debug })
}
