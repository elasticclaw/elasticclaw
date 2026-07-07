import { describe, expect, it } from "vitest"
import {
  isOptimisticMessage,
  isTerminalAssistantMessage,
  isTransientMessage,
  mergeTimelineMessages,
} from "@/lib/messages"
import type { Message } from "@/lib/types"

function msg(overrides: Partial<Message> = {}): Message {
  return {
    id: "m1",
    role: "claw",
    content: "hello",
    timestamp: new Date("2026-07-07T10:00:00Z"),
    ...overrides,
  }
}

describe("isTerminalAssistantMessage", () => {
  it("detects [DONE] and [READY_TO_COMMIT] markers on claw messages", () => {
    expect(isTerminalAssistantMessage(msg({ content: "all set [DONE]" }))).toBe(true)
    expect(isTerminalAssistantMessage(msg({ content: "[READY_TO_COMMIT] see diff" }))).toBe(true)
    expect(isTerminalAssistantMessage(msg({ content: "still working" }))).toBe(false)
  })

  it("ignores markers on non-claw roles", () => {
    expect(isTerminalAssistantMessage(msg({ role: "user", content: "[DONE]" }))).toBe(false)
  })
})

describe("isTransientMessage / isOptimisticMessage", () => {
  it("classifies ids by prefix", () => {
    expect(isTransientMessage(msg({ id: "activity-1" }))).toBe(true)
    expect(isTransientMessage(msg({ id: "live-1" }))).toBe(true)
    expect(isTransientMessage(msg({ id: "thinking-1" }))).toBe(true)
    expect(isTransientMessage(msg({ id: "uuid-1" }))).toBe(false)
    expect(isOptimisticMessage(msg({ id: "opt-1" }))).toBe(true)
    expect(isOptimisticMessage(msg({ id: "uuid-1" }))).toBe(false)
  })
})

describe("mergeTimelineMessages (optimistic merge from use-hub loadMessages)", () => {
  const t = (s: string) => new Date(`2026-07-07T${s}Z`)

  it("returns the API result when there is no cached state", () => {
    const incoming = [msg({ id: "a" }), msg({ id: "b", timestamp: t("10:01:00") })]
    expect(mergeTimelineMessages([], incoming)).toEqual(incoming)
  })

  it("dedupes messages present in both cache and API result by id", () => {
    const shared = msg({ id: "a" })
    const merged = mergeTimelineMessages([shared], [msg({ id: "a", content: "fresh" })])
    expect(merged).toHaveLength(1)
    // The API copy wins over the cached copy.
    expect(merged[0].content).toBe("fresh")
  })

  it("preserves cached durable messages beyond the API fetch window", () => {
    const old = msg({ id: "old", timestamp: t("09:00:00") })
    const merged = mergeTimelineMessages([old], [msg({ id: "new", timestamp: t("10:00:00") })])
    expect(merged.map((m) => m.id)).toEqual(["old", "new"])
  })

  it("drops transient messages from the cached state", () => {
    const existing = [
      msg({ id: "activity-1", timestamp: t("09:00:00") }),
      msg({ id: "live-2", timestamp: t("09:01:00") }),
      msg({ id: "thinking-3", timestamp: t("09:02:00") }),
      msg({ id: "durable", timestamp: t("09:03:00") }),
    ]
    const merged = mergeTimelineMessages(existing, [msg({ id: "new", timestamp: t("10:00:00") })])
    expect(merged.map((m) => m.id)).toEqual(["durable", "new"])
  })

  it("keeps in-flight optimistic messages the API has not confirmed", () => {
    const opt = msg({ id: "opt-1", role: "user", content: "typed", timestamp: t("10:02:00") })
    const merged = mergeTimelineMessages([opt], [msg({ id: "srv", timestamp: t("10:00:00") })])
    expect(merged.map((m) => m.id)).toEqual(["srv", "opt-1"])
  })

  it("drops an optimistic message once the API confirms it (same role and content)", () => {
    const opt = msg({ id: "opt-1", role: "user", content: "typed", timestamp: t("10:02:00") })
    const confirmed = msg({ id: "srv-uuid", role: "user", content: "typed", timestamp: t("10:02:01") })
    const merged = mergeTimelineMessages([opt], [confirmed])
    expect(merged.map((m) => m.id)).toEqual(["srv-uuid"])
  })

  it("keeps an optimistic message whose content matches but role differs", () => {
    const opt = msg({ id: "opt-1", role: "user", content: "same", timestamp: t("10:02:00") })
    const clawEcho = msg({ id: "srv", role: "claw", content: "same", timestamp: t("10:00:00") })
    const merged = mergeTimelineMessages([opt], [clawEcho])
    expect(merged.map((m) => m.id)).toEqual(["srv", "opt-1"])
  })

  it("sorts the merged timeline by timestamp", () => {
    const existing = [msg({ id: "cached", timestamp: t("10:01:00") })]
    const incoming = [
      msg({ id: "late", timestamp: t("10:02:00") }),
      msg({ id: "early", timestamp: t("10:00:00") }),
    ]
    const merged = mergeTimelineMessages(existing, incoming)
    expect(merged.map((m) => m.id)).toEqual(["early", "cached", "late"])
  })

  it("keeps API-first ordering for equal timestamps (stable sort)", () => {
    const ts = t("10:00:00")
    const existing = [msg({ id: "cached", timestamp: ts })]
    const incoming = [msg({ id: "api", timestamp: ts })]
    expect(mergeTimelineMessages(existing, incoming).map((m) => m.id)).toEqual(["api", "cached"])
  })
})
