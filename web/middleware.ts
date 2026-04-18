import { NextRequest, NextResponse } from "next/server"

const PUBLIC_PATHS = ["/login", "/api/auth/login", "/api/auth/logout"]

export function middleware(req: NextRequest) {
  const { pathname } = req.nextUrl

  // Allow public paths
  if (PUBLIC_PATHS.some((p) => pathname.startsWith(p))) {
    return NextResponse.next()
  }

  // Check session cookie
  const session = req.cookies.get("elasticclaw_session")?.value
  const uiToken = process.env.ELASTICCLAW_UI_TOKEN || 'admin'
  if (!session || session !== uiToken) {
    const loginUrl = new URL("/login", req.url)
    loginUrl.searchParams.set("next", pathname)
    return NextResponse.redirect(loginUrl)
  }

  return NextResponse.next()
}

export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon.ico|.*\\.png|.*\\.svg).*)"],
}
