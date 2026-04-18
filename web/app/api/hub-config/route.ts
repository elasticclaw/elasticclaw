import { NextResponse } from "next/server"

// Returns hub token to authenticated browser — session is already verified by middleware
export async function GET() {
  const token = process.env.HUB_TOKEN || process.env.NEXT_PUBLIC_HUB_TOKEN || ""
  return NextResponse.json({ token })
}
