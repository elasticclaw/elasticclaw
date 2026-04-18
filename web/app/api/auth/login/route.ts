import { NextRequest, NextResponse } from "next/server"

export async function POST(req: NextRequest) {
  const { password } = await req.json()
  // Default to 'admin' if not set, but warn loudly in dev
  const uiToken = process.env.ELASTICCLAW_UI_TOKEN || 'admin'
  if (!process.env.ELASTICCLAW_UI_TOKEN && process.env.NODE_ENV !== 'production') {
    console.warn('[elasticclaw] ELASTICCLAW_UI_TOKEN not set — defaulting to "admin"')
  }

  if (password !== uiToken) {
    return NextResponse.json({ error: "Invalid password" }, { status: 401 })
  }

  const res = NextResponse.json({ ok: true })
  res.cookies.set("elasticclaw_session", uiToken, {
    httpOnly: true,
    secure: process.env.NODE_ENV === "production",
    sameSite: "lax",
    maxAge: 60 * 60 * 24 * 30, // 30 days
    path: "/",
  })
  return res
}
