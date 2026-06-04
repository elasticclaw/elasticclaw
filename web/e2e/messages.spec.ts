import { expect, test } from "@playwright/test"
import clawsFixture from "./fixtures/claws.json"
import { mockHubApi } from "./support/mock-api"
import { installSession } from "./support/session"

test.beforeEach(async ({ page }) => {
  await installSession(page)
})

test("conversation loads message history for a selected agent", async ({ page }) => {
  await mockHubApi(page, { claws: clawsFixture.mixed })

  await page.goto("/")
  await page.locator("aside").getByText("alpha-agent").click()

  await expect(page.getByRole("heading", { name: "alpha-agent" })).toBeVisible()
  await expect(page.getByText("You")).toBeVisible()
  await expect(page.getByText("Hello from user")).toBeVisible()
  await expect(page.getByText("Hello from alpha")).toBeVisible()
})

test("conversation sends a message via the mocked API and shows it in the transcript", async ({ page }) => {
  const api = await mockHubApi(page, { claws: clawsFixture.mixed })

  await page.goto("/")
  await page.locator("aside").getByText("alpha-agent").click()
  await page.getByPlaceholder("Message agent, /stop, or attach files").fill("Draft status update")
  await page.getByRole("button", { name: "Send message" }).click()

  await expect(page.getByText("Draft status update")).toBeVisible()
  expect(api.requests.some((request) => {
    return request.method === "POST"
      && request.pathname === "/api/messages/claw-alpha"
      && typeof request.body === "object"
      && request.body !== null
      && "content" in request.body
      && request.body.content === "Draft status update"
  })).toBe(true)
})
