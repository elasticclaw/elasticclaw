import { expect, test } from "@playwright/test"
import clawsFixture from "./fixtures/claws.json"
import { mockHubApi } from "./support/mock-api"
import { installSession } from "./support/session"

test("login page renders password sign-in", async ({ page }) => {
  await mockHubApi(page)

  await page.goto("/login")

  await expect(page.getByRole("heading", { name: "ElasticClaw" })).toBeVisible()
  await expect(page.getByText("Sign in to continue")).toBeVisible()
  await expect(page.getByPlaceholder("Access token")).toBeVisible()
  await expect(page.getByRole("button", { name: "Sign in" })).toBeDisabled()
})

test("invalid password shows an error", async ({ page }) => {
  await mockHubApi(page, { auth: { validPassword: "correct-password" } })

  await page.goto("/login")
  await page.getByPlaceholder("Access token").fill("wrong-password")
  await page.getByRole("button", { name: "Sign in" }).click()

  await expect(page.getByText("Invalid password")).toBeVisible()
})

test("valid password stores the hub token and opens the dashboard", async ({ page }) => {
  await mockHubApi(page, {
    auth: { validPassword: "correct-password", hubToken: "login-token" },
    claws: clawsFixture.empty,
  })

  await page.goto("/login")
  await page.getByPlaceholder("Access token").fill("correct-password")
  await page.getByRole("button", { name: "Sign in" }).click()

  await expect(page).toHaveURL("/")
  await expect(page.getByText("No agents running")).toBeVisible()
  await expect.poll(() => page.evaluate(() => sessionStorage.getItem("ec_hub_token"))).toBe("login-token")
})

test("existing session opens the dashboard", async ({ page }) => {
  await installSession(page, { hubToken: "existing-session-token" })
  await mockHubApi(page, { claws: clawsFixture.empty })

  await page.goto("/")

  await expect(page.getByText("No agents running")).toBeVisible()
  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible()
})
