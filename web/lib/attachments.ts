// Shared types and pure utilities for the file-attachment feature.
// Consumed by the useAttachments hook and by chat/board render code.

export const MAX_FILE_BYTES = 20 * 1024 * 1024
export const MAX_FILES_PER_MSG = 10
export const ATTACHMENTS_MARKER = "\n\n[Attachments]\n"

export interface PendingAttachment {
  localId: string
  name: string
  size: number
  mimetype: string
  path?: string
  previewUrl?: string // object URL for images (revoked on remove / clear / unmount)
  status: "uploading" | "ready" | "error"
  error?: string
}

export interface ParsedAttachment {
  name: string
  path: string
  mimetype: string
  sizeLabel: string
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

// buildAttachmentsFooter renders the paths/sizes block that gets appended to
// a user message so the agent can Read the files at the paths the bridge wrote.
export function buildAttachmentsFooter(atts: PendingAttachment[]): string {
  if (atts.length === 0) return ""
  const lines = atts
    .filter((a) => a.status === "ready" && a.path)
    .map((a) => `- ${a.name} — ${a.path} (${a.mimetype}, ${formatBytes(a.size)})`)
  if (lines.length === 0) return ""
  return `${ATTACHMENTS_MARKER}${lines.join("\n")}`
}

// splitAttachmentsFooter extracts the attachments block from stored message
// content so history can render chips without a schema change. Accepts either
// the embedded form ("body\n\n[Attachments]\n…") or a body-less form where the
// marker sits at the start of the message (the hub trims leading whitespace).
export function splitAttachmentsFooter(content: string): { body: string; attachments: ParsedAttachment[] } {
  let idx = content.indexOf(ATTACHMENTS_MARKER)
  let markerLen = ATTACHMENTS_MARKER.length
  if (idx < 0 && content.startsWith("[Attachments]\n")) {
    idx = 0
    markerLen = "[Attachments]\n".length
  }
  if (idx < 0) return { body: content, attachments: [] }
  const body = content.slice(0, idx)
  const tail = content.slice(idx + markerLen)
  const atts: ParsedAttachment[] = []
  for (const line of tail.split("\n")) {
    // Expected: "- name — path (mimetype, size)". Both name and path captures
    // are greedy so backtracking anchors on the *last* " — " and " (" on the
    // line. The bridge's sanitizeFilename strips " — " from stored paths, so
    // embedded em-dashes in the user-visible name (e.g. "notes — draft.pdf")
    // round-trip correctly. Greedy path also handles "(" inside filenames.
    const m = /^-\s+(.+)\s+—\s+(.+)\s+\(([^,]+),\s*([^)]+)\)\s*$/.exec(line)
    if (!m) continue
    atts.push({ name: m[1], path: m[2], mimetype: m[3], sizeLabel: m[4] })
  }
  if (atts.length === 0) return { body: content, attachments: [] }
  return { body, attachments: atts }
}
