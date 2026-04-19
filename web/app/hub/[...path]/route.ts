import { NextRequest, NextResponse } from "next/server"

// Runtime proxy to the hub — reads ELASTICCLAW_HUB_URL at request time,
// not at build time (unlike next.config.mjs rewrites which are baked in).
const getHubUrl = () =>
  process.env.ELASTICCLAW_HUB_URL ||
  process.env.HUB_URL ||
  "http://localhost:8080"

async function proxy(req: NextRequest, path: string[]) {
  const hubUrl = getHubUrl()
  const upstream = `${hubUrl}/${path.join("/")}${req.nextUrl.search}`

  // Forward the request to the hub
  const headers = new Headers(req.headers)
  headers.delete("host")

  const res = await fetch(upstream, {
    method: req.method,
    headers,
    body: req.method !== "GET" && req.method !== "HEAD" ? req.body : undefined,
    // @ts-expect-error - duplex needed for streaming body
    duplex: "half",
  })

  const responseHeaders = new Headers(res.headers)
  // Allow WebSocket upgrade headers through
  responseHeaders.delete("content-encoding")

  return new NextResponse(res.body, {
    status: res.status,
    statusText: res.statusText,
    headers: responseHeaders,
  })
}

export async function GET(req: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  return proxy(req, path)
}

export async function POST(req: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  return proxy(req, path)
}

export async function DELETE(req: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  return proxy(req, path)
}

export async function PATCH(req: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  return proxy(req, path)
}

export async function OPTIONS(req: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  return proxy(req, path)
}
