import { describe, expect, it } from "vitest"
import {
  ATTACHMENTS_MARKER,
  buildAttachmentsFooter,
  formatBytes,
  splitAttachmentsFooter,
  type PendingAttachment,
} from "@/lib/attachments"

function att(overrides: Partial<PendingAttachment> = {}): PendingAttachment {
  return {
    localId: "l1",
    name: "report.pdf",
    size: 2048,
    mimetype: "application/pdf",
    path: "/workspace/uploads/report.pdf",
    status: "ready",
    ...overrides,
  }
}

describe("formatBytes", () => {
  it("formats bytes below 1 KB", () => {
    expect(formatBytes(0)).toBe("0 B")
    expect(formatBytes(1023)).toBe("1023 B")
  })

  it("formats kilobytes with one decimal", () => {
    expect(formatBytes(1024)).toBe("1.0 KB")
    expect(formatBytes(1536)).toBe("1.5 KB")
  })

  it("formats megabytes with one decimal", () => {
    expect(formatBytes(1024 * 1024)).toBe("1.0 MB")
    expect(formatBytes(20 * 1024 * 1024)).toBe("20.0 MB")
  })
})

describe("buildAttachmentsFooter", () => {
  it("returns an empty string for no attachments", () => {
    expect(buildAttachmentsFooter([])).toBe("")
  })

  it("returns an empty string when no attachment is ready with a path", () => {
    expect(
      buildAttachmentsFooter([att({ status: "uploading" }), att({ path: undefined })])
    ).toBe("")
  })

  it("renders one line per ready attachment", () => {
    const footer = buildAttachmentsFooter([
      att(),
      att({ localId: "l2", name: "img.png", size: 1024, mimetype: "image/png", path: "/workspace/uploads/img.png" }),
    ])
    expect(footer).toBe(
      `${ATTACHMENTS_MARKER}- report.pdf — /workspace/uploads/report.pdf (application/pdf, 2.0 KB)\n` +
        `- img.png — /workspace/uploads/img.png (image/png, 1.0 KB)`
    )
  })

  it("skips attachments that are not ready", () => {
    const footer = buildAttachmentsFooter([att(), att({ localId: "l2", name: "x.txt", status: "error" })])
    expect(footer).toContain("report.pdf")
    expect(footer).not.toContain("x.txt")
  })
})

describe("splitAttachmentsFooter", () => {
  it("returns the content untouched when there is no marker", () => {
    expect(splitAttachmentsFooter("just a message")).toEqual({
      body: "just a message",
      attachments: [],
    })
  })

  it("round-trips a footer built by buildAttachmentsFooter", () => {
    const content = `hello world${buildAttachmentsFooter([att()])}`
    const { body, attachments } = splitAttachmentsFooter(content)
    expect(body).toBe("hello world")
    expect(attachments).toEqual([
      {
        name: "report.pdf",
        path: "/workspace/uploads/report.pdf",
        mimetype: "application/pdf",
        sizeLabel: "2.0 KB",
      },
    ])
  })

  it("accepts the body-less leading-marker form", () => {
    const content = "[Attachments]\n- a.txt — /tmp/a.txt (text/plain, 5 B)"
    const { body, attachments } = splitAttachmentsFooter(content)
    expect(body).toBe("")
    expect(attachments).toEqual([
      { name: "a.txt", path: "/tmp/a.txt", mimetype: "text/plain", sizeLabel: "5 B" },
    ])
  })

  it("anchors on the last em-dash so em-dashes in names round-trip", () => {
    const content = `${ATTACHMENTS_MARKER}- notes — draft.pdf — /tmp/notes-draft.pdf (application/pdf, 1.0 KB)`
    const { attachments } = splitAttachmentsFooter(content)
    expect(attachments).toEqual([
      {
        name: "notes — draft.pdf",
        path: "/tmp/notes-draft.pdf",
        mimetype: "application/pdf",
        sizeLabel: "1.0 KB",
      },
    ])
  })

  it("handles parentheses inside filenames", () => {
    const content = `body${ATTACHMENTS_MARKER}- shot (1).png — /tmp/shot (1).png (image/png, 3.0 KB)`
    const { attachments } = splitAttachmentsFooter(content)
    expect(attachments).toEqual([
      { name: "shot (1).png", path: "/tmp/shot (1).png", mimetype: "image/png", sizeLabel: "3.0 KB" },
    ])
  })

  it("falls back to the full content when no line after the marker parses", () => {
    const content = `body${ATTACHMENTS_MARKER}garbage that is not an attachment line`
    expect(splitAttachmentsFooter(content)).toEqual({ body: content, attachments: [] })
  })

  it("skips unparseable lines but keeps valid ones", () => {
    const content = `body${ATTACHMENTS_MARKER}not a line\n- a.txt — /tmp/a.txt (text/plain, 5 B)`
    const { body, attachments } = splitAttachmentsFooter(content)
    expect(body).toBe("body")
    expect(attachments).toHaveLength(1)
    expect(attachments[0].name).toBe("a.txt")
  })
})
