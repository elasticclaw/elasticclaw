import type { Page, Route } from "@playwright/test"
import clawsFixture from "../fixtures/claws.json"
import messagesFixture from "../fixtures/messages.json"
import workspacesFixture from "../fixtures/workspaces.json"

type ApiClaw = typeof clawsFixture.mixed[number]
type ApiMessage = typeof messagesFixture["claw-alpha"][number]
type Workspace = typeof workspacesFixture[number]

interface AuthScenario {
  validPassword?: string
  hubToken?: string
  githubOAuthEnabled?: boolean
  passwordAuthEnabled?: boolean
}

interface MockHubApiScenario {
  auth?: AuthScenario
  claws?: ApiClaw[]
  messages?: Record<string, ApiMessage[]>
  workspaces?: Workspace[]
  isAdmin?: boolean
  settingsStatus?: {
    hasProvider: boolean
    hasLLMKey: boolean
  }
}

export interface ApiRequestRecord {
  method: string
  pathname: string
  body: unknown
}

export interface MockHubApi {
  requests: ApiRequestRecord[]
  claws: ApiClaw[]
  messages: Record<string, ApiMessage[]>
}

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  })
}

function noContent(route: Route) {
  return route.fulfill({ status: 204, body: "" })
}

function apiMessage(clawId: string, content: string): ApiMessage {
  return {
    id: `msg-${clawId}-${Date.now()}`,
    claw_id: clawId,
    tenant_id: "tenant-test",
    role: "user",
    content,
    created_at: new Date().toISOString(),
  }
}

export async function mockHubApi(page: Page, scenario: MockHubApiScenario = {}): Promise<MockHubApi> {
  const state: MockHubApi = {
    requests: [],
    claws: [...(scenario.claws ?? clawsFixture.empty)],
    messages: structuredClone(scenario.messages ?? messagesFixture),
  }
  const auth = scenario.auth ?? {}
  const validPassword = auth.validPassword ?? "correct-password"
  const hubToken = auth.hubToken ?? "mock-hub-token"

  await page.route("**/api/**", async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    const pathname = url.pathname
    let body: unknown = null

    if (method !== "GET") {
      try {
        body = request.postDataJSON()
      } catch {
        body = request.postData()
      }
    }

    state.requests.push({ method, pathname, body })

    if (pathname === "/api/auth/config" && method === "GET") {
      return json(route, 200, {
        github_oauth_enabled: auth.githubOAuthEnabled ?? false,
        password_auth_enabled: auth.passwordAuthEnabled ?? true,
      })
    }

    if (pathname === "/api/auth/login" && method === "POST") {
      const password = typeof body === "object" && body && "password" in body ? body.password : ""
      if (password !== validPassword) return json(route, 401, { error: "invalid password" })
      return json(route, 200, { hubToken })
    }

    if (pathname === "/api/auth/me" && method === "GET") {
      return json(route, 200, { is_admin: scenario.isAdmin ?? true })
    }

    if (pathname === "/api/auth/logout" && method === "POST") {
      return noContent(route)
    }

    if (pathname === "/api/settings/status" && method === "GET") {
      return json(route, 200, scenario.settingsStatus ?? { hasProvider: true, hasLLMKey: true })
    }

    if (pathname === "/api/hub-config" && method === "GET") {
      return json(route, 200, { token: hubToken, hubUrl: "", version: "e2e-test" })
    }

    if (pathname === "/api/claws" && method === "GET") {
      return json(route, 200, state.claws)
    }

    if (pathname === "/api/claws" && method === "POST") {
      const req = body as Partial<ApiClaw> & { provider?: string }
      const claw: ApiClaw = {
        id: `claw-${Date.now()}`,
        name: req.name || "new-agent",
        template: req.template || "manual",
        status: "provisioning",
        last_seen: new Date().toISOString(),
        created_at: new Date().toISOString(),
        tenant_id: "tenant-test",
        context_usage: 0,
        tags: [],
        color: "blue",
      }
      state.claws = [claw, ...state.claws]
      return json(route, 200, claw)
    }

    const clawPrsMatch = pathname.match(/^\/api\/claws\/([^/]+)\/prs$/)
    if (clawPrsMatch && method === "GET") {
      return json(route, 200, [])
    }

    const clawMatch = pathname.match(/^\/api\/claws\/([^/]+)$/)
    if (clawMatch && method === "DELETE") {
      const clawId = decodeURIComponent(clawMatch[1])
      state.claws = state.claws.filter((claw) => claw.id !== clawId)
      delete state.messages[clawId]
      return noContent(route)
    }

    if (clawMatch && method === "PATCH") {
      const clawId = decodeURIComponent(clawMatch[1])
      const patch = body as Partial<ApiClaw>
      state.claws = state.claws.map((claw) => claw.id === clawId ? { ...claw, ...patch } : claw)
      return noContent(route)
    }

    const messagesMatch = pathname.match(/^\/api\/messages\/([^/]+)$/)
    if (messagesMatch && method === "GET") {
      const clawId = decodeURIComponent(messagesMatch[1])
      return json(route, 200, state.messages[clawId] ?? [])
    }

    if (messagesMatch && method === "POST") {
      const clawId = decodeURIComponent(messagesMatch[1])
      const content = typeof body === "object" && body && "content" in body ? String(body.content) : ""
      const message = apiMessage(clawId, content)
      state.messages[clawId] = [...(state.messages[clawId] ?? []), message]
      return json(route, 200, message)
    }

    if (pathname === "/api/workspaces" && method === "GET") {
      return json(route, 200, scenario.workspaces ?? [])
    }

    const triggerMatch = pathname.match(/^\/api\/workspaces\/([^/]+)\/workflows\/([^/]+)\/trigger$/)
    if (triggerMatch && method === "POST") {
      return json(route, 200, { claw_id: "claw-triggered", status: "created" })
    }

    if (pathname === "/api/settings" && method === "GET") {
      return json(route, 200, {
        llmKeys: [],
        providers: {},
        github: [],
        sshPublicKeys: [],
      })
    }

    return json(route, 404, { error: `No mock for ${method} ${pathname}` })
  })

  return state
}
