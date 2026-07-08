import { describe, expect, it } from "vitest"
import { API_CLAW_STATUSES, isDeletedClaw, mapApiStatus } from "./mappers"
import type { ApiClawStatus, ClawStatus } from "./types"

// Expected presentation for every wire status the hub can emit.
// If a status is added to api/openapi.yaml, API_CLAW_STATUSES stops compiling
// until it is listed, and this table forces a decision on how it renders.
const EXPECTED_PRESENTATION: Record<ApiClawStatus, ClawStatus> = {
  pending: "provisioning",
  provisioning: "provisioning",
  starting: "provisioning",
  connected: "connected",
  idle: "idle",
  offline: "offline",
  error: "error",
  deleted: "offline", // never rendered — deleted claws are filtered out
}

describe("mapApiStatus", () => {
  it("covers every value of the generated ClawStatus enum", () => {
    expect(Object.keys(EXPECTED_PRESENTATION).sort()).toEqual(
      [...API_CLAW_STATUSES].sort()
    )
  })

  it.each([...API_CLAW_STATUSES])("maps %s to its presentation status", (status) => {
    expect(mapApiStatus(status)).toBe(EXPECTED_PRESENTATION[status])
  })

  it("renders starting like provisioning (spinner)", () => {
    expect(mapApiStatus("starting")).toBe(mapApiStatus("provisioning"))
  })

  it("throws on values outside the generated enum", () => {
    expect(() => mapApiStatus("bogus" as ApiClawStatus)).toThrow(/Unhandled value/)
  })
})

describe("isDeletedClaw", () => {
  it("is true only for deleted", () => {
    for (const status of API_CLAW_STATUSES) {
      expect(isDeletedClaw({ status })).toBe(status === "deleted")
    }
  })
})
