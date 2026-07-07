// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

// jsdom does not implement BroadcastChannel, so install a deterministic fake
// that delivers messages synchronously to the other channels with the same
// name (a real BroadcastChannel never delivers to the sender).
class FakeBroadcastChannel {
  static instances: FakeBroadcastChannel[] = []
  name: string
  onmessage: ((event: MessageEvent) => void) | null = null
  private listeners = new Set<(event: MessageEvent) => void>()
  private closed = false

  constructor(name: string) {
    this.name = name
    FakeBroadcastChannel.instances.push(this)
  }

  postMessage(data: unknown): void {
    for (const other of FakeBroadcastChannel.instances) {
      if (other === this || other.closed || other.name !== this.name) continue
      other.deliver(data)
    }
  }

  deliver(data: unknown): void {
    const event = { data } as MessageEvent
    this.onmessage?.(event)
    for (const listener of this.listeners) listener(event)
  }

  addEventListener(_type: string, listener: (event: MessageEvent) => void): void {
    this.listeners.add(listener)
  }

  removeEventListener(_type: string, listener: (event: MessageEvent) => void): void {
    this.listeners.delete(listener)
  }

  close(): void {
    this.closed = true
  }

  static reset(): void {
    FakeBroadcastChannel.instances = []
  }
}

type AuthStorage = typeof import("@/lib/auth-storage")

// The module keeps a singleton channel and registers it at import time, so
// each test gets a fresh copy via resetModules + dynamic import.
async function importAuthStorage(): Promise<AuthStorage> {
  vi.resetModules()
  return await import("@/lib/auth-storage")
}

beforeEach(() => {
  sessionStorage.clear()
  FakeBroadcastChannel.reset()
  vi.stubGlobal("BroadcastChannel", FakeBroadcastChannel)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe("token storage", () => {
  it("stores and reads the GitHub and hub tokens from sessionStorage", async () => {
    const auth = await importAuthStorage()
    auth.setGitHubToken("gh-token")
    auth.setHubToken("hub-token")
    expect(auth.getGitHubToken()).toBe("gh-token")
    expect(auth.getHubToken()).toBe("hub-token")
    expect(sessionStorage.getItem("ec_github_token")).toBe("gh-token")
    expect(sessionStorage.getItem("ec_hub_token")).toBe("hub-token")
  })

  it("getAuthToken prefers the GitHub token over the hub token", async () => {
    const auth = await importAuthStorage()
    expect(auth.getAuthToken()).toBeNull()
    auth.setHubToken("hub-token")
    expect(auth.getAuthToken()).toBe("hub-token")
    auth.setGitHubToken("gh-token")
    expect(auth.getAuthToken()).toBe("gh-token")
  })

  it("clearAuthTokens removes both tokens", async () => {
    const auth = await importAuthStorage()
    auth.setGitHubToken("gh-token")
    auth.setHubToken("hub-token")
    auth.clearAuthTokens()
    expect(auth.getAuthToken()).toBeNull()
    expect(sessionStorage.getItem("ec_github_token")).toBeNull()
    expect(sessionStorage.getItem("ec_hub_token")).toBeNull()
  })
})

describe("cross-tab broadcast", () => {
  function otherTab(): FakeBroadcastChannel {
    // The module opens its channel at import time, so a channel created after
    // the import acts as "another tab" on the same channel name.
    return new FakeBroadcastChannel("elasticclaw-auth")
  }

  it("broadcasts token updates to other tabs", async () => {
    const auth = await importAuthStorage()
    const tab = otherTab()
    const received: unknown[] = []
    tab.onmessage = (event) => received.push(event.data)
    auth.setGitHubToken("gh-token")
    expect(received).toEqual([
      { type: "auth-token-updated", githubToken: "gh-token", hubToken: null },
    ])
  })

  it("broadcasts a cleared event on clearAuthTokens", async () => {
    const auth = await importAuthStorage()
    const tab = otherTab()
    const received: Array<{ type: string }> = []
    tab.onmessage = (event) => received.push(event.data)
    auth.clearAuthTokens()
    expect(received).toEqual([{ type: "auth-token-cleared" }])
  })

  it("answers auth-token-request with the stored tokens", async () => {
    const auth = await importAuthStorage()
    auth.setHubToken("hub-token")
    const tab = otherTab()
    const received: Array<{ type: string }> = []
    tab.addEventListener("message", (event) => received.push(event.data))
    tab.postMessage({ type: "auth-token-request" })
    expect(received).toEqual([
      { type: "auth-token-response", githubToken: null, hubToken: "hub-token" },
    ])
  })

  it("stays silent on auth-token-request when no token is stored", async () => {
    await importAuthStorage()
    const tab = otherTab()
    const received: unknown[] = []
    tab.addEventListener("message", (event) => received.push(event.data))
    tab.postMessage({ type: "auth-token-request" })
    expect(received).toEqual([])
  })

  it("adopts tokens broadcast by another tab", async () => {
    const auth = await importAuthStorage()
    const tab = otherTab()
    tab.postMessage({ type: "auth-token-updated", githubToken: "gh-from-tab", hubToken: null })
    expect(auth.getGitHubToken()).toBe("gh-from-tab")
  })

  it("clears tokens when another tab broadcasts auth-token-cleared", async () => {
    const auth = await importAuthStorage()
    auth.setHubToken("hub-token")
    const tab = otherTab()
    tab.postMessage({ type: "auth-token-cleared" })
    expect(auth.getAuthToken()).toBeNull()
  })
})

describe("requestAuthToken", () => {
  it("resolves immediately when a token already exists", async () => {
    const auth = await importAuthStorage()
    auth.setGitHubToken("gh-token")
    await expect(auth.requestAuthToken()).resolves.toBe("gh-token")
  })

  it("resolves with a token supplied by another tab", async () => {
    const auth = await importAuthStorage()
    const tab = otherAnsweringTab("gh-from-tab")
    await expect(auth.requestAuthToken(1000)).resolves.toBe("gh-from-tab")
    tab.close()
  })

  it("resolves null after the timeout when no tab answers", async () => {
    const auth = await importAuthStorage()
    await expect(auth.requestAuthToken(10)).resolves.toBeNull()
  })

  function otherAnsweringTab(githubToken: string): FakeBroadcastChannel {
    const tab = new FakeBroadcastChannel("elasticclaw-auth")
    tab.onmessage = (event) => {
      if ((event.data as { type?: string })?.type === "auth-token-request") {
        tab.postMessage({ type: "auth-token-response", githubToken, hubToken: null })
      }
    }
    return tab
  }
})

describe("OAuth flow state", () => {
  it("stores, reads, and removes OAuth state and next URL", async () => {
    const auth = await importAuthStorage()
    expect(auth.getOAuthState()).toBeNull()
    auth.setOAuthState("csrf-123")
    expect(auth.getOAuthState()).toBe("csrf-123")
    auth.removeOAuthState()
    expect(auth.getOAuthState()).toBeNull()

    expect(auth.getOAuthNext()).toBeNull()
    auth.setOAuthNext("/settings")
    expect(auth.getOAuthNext()).toBe("/settings")
    auth.removeOAuthNext()
    expect(auth.getOAuthNext()).toBeNull()
  })
})
