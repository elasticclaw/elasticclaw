export function safeNextPath(next: string | null): string | null {
  if (!next || !next.startsWith("/") || next.startsWith("//") || /[\\\x00-\x20\x7f]/.test(next)) return null
  return next
}

export function loginRedirectPath(path: string): string {
  const next = safeNextPath(path)
  return next ? `/login?next=${encodeURIComponent(next)}` : "/login"
}
