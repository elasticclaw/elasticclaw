import { expect, test } from "@playwright/test"
import clawsFixture from "./fixtures/claws.json"
import workspacesFixture from "./fixtures/workspaces.json"
import { mockHubApi } from "./support/mock-api"
import { installSession } from "./support/session"

test.beforeEach(async ({ page }) => {
  await installSession(page)
})

test("dashboard shows the empty state when no agents exist", async ({ page }) => {
  await mockHubApi(page, { claws: clawsFixture.empty })

  await page.goto("/")

  await expect(page.getByText("0 Active Agents")).toBeVisible()
  await expect(page.getByText("No agents running")).toBeVisible()
  await expect(page.locator("aside").getByText("No agents found")).toBeVisible()
})

test("dashboard lists agents, filters sidebar results, and opens a conversation", async ({ page }) => {
  await mockHubApi(page, { claws: clawsFixture.mixed })

  await page.goto("/")
  const sidebar = page.locator("aside")

  await expect(page.getByText("4 Active Agents")).toBeVisible()
  await expect(sidebar.getByText("alpha-agent")).toBeVisible()
  await expect(sidebar.getByText("docs-agent")).toBeVisible()
  await expect(page.getByText("starting...")).toBeVisible()

  await sidebar.getByPlaceholder("Filter by name or workflow...").fill("alpha")
  await expect(sidebar.getByText("alpha-agent")).toBeVisible()
  await expect(sidebar.getByText("docs-agent")).not.toBeVisible()

  await sidebar.getByPlaceholder("Filter by name or workflow...").fill("")
  await sidebar.getByRole("button", { name: "Tags" }).click()
  await page.getByRole("menuitem", { name: "billing" }).click()
  await expect(sidebar.getByText("alpha-agent")).toBeVisible()
  await expect(sidebar.getByText("docs-agent")).not.toBeVisible()

  await sidebar.getByText("alpha-agent").click()
  await expect(page.getByRole("heading", { name: "alpha-agent" })).toBeVisible()
  await expect(page.getByText("connected")).toBeVisible()
  await expect(page.getByText("Hello from alpha")).toBeVisible()
  await expect(page.getByPlaceholder("Message agent, /stop, or attach files")).toBeVisible()
})

test("admin sees settings while non-admin does not", async ({ page }) => {
  await mockHubApi(page, { claws: clawsFixture.empty, isAdmin: false })

  await page.goto("/")

  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible()
  await expect(page.getByTitle("Settings")).not.toBeVisible()
})

test("manual workflow trigger opens and submits", async ({ page }) => {
  const api = await mockHubApi(page, {
    claws: clawsFixture.empty,
    workspaces: workspacesFixture,
  })

  await page.goto("/")
  await page.getByTitle("Create Agent").click()
  await expect(page.getByRole("heading", { name: "Trigger manual-smoke" })).toBeVisible()
  await expect(page.getByRole("dialog").locator("input").first()).toHaveValue("Run smoke workflow")
  await page.getByRole("button", { name: "Confirm" }).click()

  await expect(page.getByRole("heading", { name: "Trigger manual-smoke" })).not.toBeVisible()
  expect(api.requests.some((request) => request.method === "POST" && request.pathname === "/api/workspaces/default/workflows/manual-smoke/trigger")).toBe(true)
})
