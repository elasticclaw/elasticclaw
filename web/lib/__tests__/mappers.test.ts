import { afterEach, describe, expect, it, vi } from "vitest"
import {
  CLAW_COLORS,
  computeUptime,
  mapApiClaw,
  mapApiMessage,
  mapApiStatus,
  parseTagsFromName,
} from "@/lib/mappers"
import type { ApiClaw, ApiMessage } from "@/lib/types"

function apiClaw(overrides: Partial<ApiClaw> = {}): ApiClaw {
  return {
    id: "claw-1",
    name: "my-claw",
    template: "default",
    status: "connected",
    last_seen: "2026-07-07T10:00:00Z",
    created_at: "2026-07-07T09:00:00Z",
    tenant_id: "tenant-1",
    ...overrides,
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe("parseTagsFromName", () => {
  it("extracts key=value pairs from the name", () => {
    expect(parseTagsFromName("my-claw env=prod team=core")).toEqual({
      env: "prod",
      team: "core",
    })
  })

  it("returns an empty object when there are no tags", () => {
    expect(parseTagsFromName("just-a-name")).toEqual({})
  })

  it("ignores parts with an empty key or value", () => {
    expect(parseTagsFromName("name =prod env= key=val")).toEqual({ key: "val" })
  })

  it("keeps everything after the first equals sign as the value", () => {
    expect(parseTagsFromName("name expr=a=b")).toEqual({ expr: "a=b" })
  })
})

describe("computeUptime", () => {
  it("computes seconds since created_at for connected claws", () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date("2026-07-07T10:00:00Z"))
    expect(computeUptime(apiClaw({ created_at: "2026-07-07T09:59:00Z" }))).toBe(60)
  })

  it("computes uptime for idle claws", () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date("2026-07-07T10:00:00Z"))
    expect(computeUptime(apiClaw({ status: "idle", created_at: "2026-07-07T09:00:00Z" }))).toBe(3600)
  })

  it("returns 0 for non-running statuses", () => {
    expect(computeUptime(apiClaw({ status: "offline" }))).toBe(0)
    expect(computeUptime(apiClaw({ status: "provisioning" }))).toBe(0)
    expect(computeUptime(apiClaw({ status: "error" }))).toBe(0)
  })

  it("returns 0 for an unparseable created_at", () => {
    expect(computeUptime(apiClaw({ created_at: "not-a-date" }))).toBe(0)
  })

  it("clamps negative uptime (created_at in the future) to 0", () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date("2026-07-07T10:00:00Z"))
    expect(computeUptime(apiClaw({ created_at: "2026-07-07T11:00:00Z" }))).toBe(0)
  })
})

describe("mapApiStatus", () => {
  it("maps passthrough statuses", () => {
    expect(mapApiStatus("connected")).toBe("connected")
    expect(mapApiStatus("idle")).toBe("idle")
    expect(mapApiStatus("error")).toBe("error")
  })

  it("maps starting to provisioning", () => {
    expect(mapApiStatus("provisioning")).toBe("provisioning")
    expect(mapApiStatus("starting")).toBe("provisioning")
  })

  it("maps unknown statuses to offline", () => {
    expect(mapApiStatus("offline")).toBe("offline")
    // Statuses the backend may add later must degrade to offline, not crash.
    expect(mapApiStatus("deleted" as ApiClaw["status"])).toBe("offline")
  })
})

describe("mapApiClaw", () => {
  it("maps API fields with defaults", () => {
    const claw = mapApiClaw(apiClaw({ status: "offline", tags: ["a"], context_usage: 42 }))
    expect(claw).toMatchObject({
      id: "claw-1",
      name: "my-claw",
      template: "default",
      status: "offline",
      uptime: 0,
      unreadCount: 0,
      isStreaming: false,
      pinned: false,
      tags: ["a"],
      contextUsage: 42,
      tenant_id: "tenant-1",
    })
  })

  it("assigns a deterministic auto color from the name when unset", () => {
    const a = mapApiClaw(apiClaw({ color: undefined }))
    const b = mapApiClaw(apiClaw({ color: undefined }))
    expect(a.color).toBe(b.color)
    expect(CLAW_COLORS).toContain(a.color)
  })

  it("prefers the explicit API color over the auto color", () => {
    expect(mapApiClaw(apiClaw({ color: "teal" })).color).toBe("teal")
  })

  it("lets overrides win over API-derived values", () => {
    const claw = mapApiClaw(apiClaw(), {
      status: "error",
      uptime: 123,
      unreadCount: 7,
      pinned: true,
      color: "rose",
      tags: ["x"],
    })
    expect(claw.status).toBe("error")
    expect(claw.uptime).toBe(123)
    expect(claw.unreadCount).toBe(7)
    expect(claw.pinned).toBe(true)
    expect(claw.color).toBe("rose")
    expect(claw.tags).toEqual(["x"])
  })
})

describe("mapApiMessage", () => {
  function apiMessage(overrides: Partial<ApiMessage> = {}): ApiMessage {
    return {
      id: "msg-1",
      claw_id: "claw-1",
      tenant_id: "tenant-1",
      role: "claw",
      content: "hello",
      created_at: "2026-07-07T10:00:00Z",
      ...overrides,
    }
  }

  it("maps plain messages and parses the timestamp", () => {
    const msg = mapApiMessage(apiMessage({ format: "pre" }))
    expect(msg.id).toBe("msg-1")
    expect(msg.role).toBe("claw")
    expect(msg.content).toBe("hello")
    expect(msg.format).toBe("pre")
    expect(msg.timestamp).toEqual(new Date("2026-07-07T10:00:00Z"))
    expect(msg.activity).toBeUndefined()
  })

  it("parses activity payloads out of the format field", () => {
    const msg = mapApiMessage(
      apiMessage({ role: "activity", format: 'activity:{"kind":"tool","tool":"Bash"}' })
    )
    expect(msg.activity).toEqual({ kind: "tool", tool: "Bash" })
    expect(msg.format).toBeUndefined()
  })

  it("parses activity_summary payloads out of the format field", () => {
    const msg = mapApiMessage(
      apiMessage({ role: "activity_summary", format: 'activity_summary:{"count":3}' })
    )
    expect(msg.activitySummary).toEqual({ count: 3 })
    expect(msg.format).toBeUndefined()
  })

  it("swallows malformed activity JSON instead of throwing", () => {
    const msg = mapApiMessage(apiMessage({ role: "activity", format: "activity:{not json" }))
    expect(msg.activity).toBeUndefined()
    expect(msg.format).toBeUndefined()
  })

  it("does not parse activity payloads for non-activity roles", () => {
    const msg = mapApiMessage(apiMessage({ role: "claw", format: 'activity:{"kind":"tool"}' }))
    expect(msg.activity).toBeUndefined()
    // The raw activity format is still hidden from render code.
    expect(msg.format).toBeUndefined()
  })
})
