import { describe, expect, it } from "vitest"
import { isTransientMessage, mergeMessageTimeline } from "./use-message-cache"
import { describeWsUrl } from "./use-websocket"
import type { Message } from "@/lib/types"

function msg(id: string, content: string, ts: number, role: Message["role"] = "claw"): Message {
  return { id, role, content, timestamp: new Date(ts) }
}

describe("isTransientMessage", () => {
  it("flags live segments, activity rows and thinking placeholders", () => {
    expect(isTransientMessage(msg("live-segment-c1-1", "x", 0))).toBe(true)
    expect(isTransientMessage(msg("activity-c1-1", "x", 0))).toBe(true)
    expect(isTransientMessage(msg("thinking-1", "x", 0))).toBe(true)
  })

  it("does not flag durable or optimistic messages", () => {
    expect(isTransientMessage(msg("a3f9", "x", 0))).toBe(false)
    expect(isTransientMessage(msg("opt-c1-1", "x", 0))).toBe(false)
  })
})

describe("mergeMessageTimeline", () => {
  it("keeps cached durable messages missing from the API result (beyond fetch limit)", () => {
    const cached = [msg("old-1", "ancient", 1000), msg("new-1", "recent", 3000)]
    const fresh = [msg("new-1", "recent", 3000), msg("new-2", "newer", 4000)]
    const merged = mergeMessageTimeline(cached, fresh)
    expect(merged.map((m) => m.id)).toEqual(["old-1", "new-1", "new-2"])
  })

  it("drops transient messages from the cached side", () => {
    const cached = [msg("live-segment-c1-1", "stream", 1000), msg("activity-c1-1", "tool", 2000, "activity")]
    const fresh = [msg("real-1", "final", 3000)]
    expect(mergeMessageTimeline(cached, fresh).map((m) => m.id)).toEqual(["real-1"])
  })

  it("re-appends in-flight optimistic messages the API has not confirmed", () => {
    const cached = [msg("opt-c1-1", "hello", 5000, "user")]
    const fresh = [msg("real-1", "earlier", 1000)]
    const merged = mergeMessageTimeline(cached, fresh)
    expect(merged.map((m) => m.id)).toEqual(["real-1", "opt-c1-1"])
  })

  it("drops optimistic messages once the API returns the confirmed copy", () => {
    const cached = [msg("opt-c1-1", "hello", 5000, "user")]
    const fresh = [msg("real-1", "hello", 5000, "user")]
    expect(mergeMessageTimeline(cached, fresh).map((m) => m.id)).toEqual(["real-1"])
  })

  it("sorts the merged timeline by timestamp", () => {
    const cached = [msg("cached-late", "z", 9000)]
    const fresh = [msg("fresh-early", "a", 1000), msg("fresh-mid", "b", 5000)]
    expect(mergeMessageTimeline(cached, fresh).map((m) => m.id)).toEqual([
      "fresh-early",
      "fresh-mid",
      "cached-late",
    ])
  })
})

describe("describeWsUrl", () => {
  it("redacts the token query parameter", () => {
    expect(describeWsUrl("ws://hub.local/api/ws?token=secret123")).toBe(
      "ws://hub.local/api/ws?token=%5Bredacted%5D"
    )
  })

  it("redacts tokens even when the URL does not parse", () => {
    expect(describeWsUrl("not a url token=secret&x=1")).toBe("not a url token=[redacted]&x=1")
  })

  it("leaves token-free URLs untouched", () => {
    expect(describeWsUrl("wss://hub.local/api/ws")).toBe("wss://hub.local/api/ws")
  })
})
