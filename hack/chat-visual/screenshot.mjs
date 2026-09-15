#!/usr/bin/env node
// Zero-dependency CDP screenshotter for the agent chat UI.
//
//   node hack/chat-visual/screenshot.mjs --label before
//
// Requires the seeded hub (hack/chat-visual/run-hub.sh) and `npm --prefix web
// run dev` to be running. Writes PNGs to hack/chat-visual/out/<label>-*.png.
//
// Node 24 has a global WebSocket, so this needs no packages. Puppeteer is
// deliberately avoided: the repo has no browser dependency and this must stay
// runnable from a bare checkout.

import { spawn } from "node:child_process"
import { mkdirSync, writeFileSync, rmSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const HERE = dirname(fileURLToPath(import.meta.url))
const OUT_DIR = join(HERE, "out")

const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
const DEBUG_PORT = 9222
const USER_DATA_DIR = "/tmp/ec-chat-visual"

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback
}

const LABEL = arg("label", "shot")
// localhost, never 127.0.0.1 — see memory next16-clock-in-render-suspends.
const APP_URL = arg("app", "http://localhost:3000")
const HUB_URL = arg("hub", "http://localhost:8085")
const HUB_TOKEN = arg("token", "test-token")
// Substring of the claw name whose conversation we want on screen.
const CLAW_MATCH = arg("claw", "token-refresh-race")
const KEEP_CHROME = process.argv.includes("--keep-chrome")

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// ─── chrome ──────────────────────────────────────────────────────────────────

async function chromeVersion() {
  const res = await fetch(`http://localhost:${DEBUG_PORT}/json/version`)
  return res.json()
}

async function launchChrome() {
  try {
    const v = await chromeVersion()
    console.log(`[chrome] reusing running instance: ${v.Browser}`)
    return null
  } catch {}
  rmSync(USER_DATA_DIR, { recursive: true, force: true })
  const child = spawn(CHROME, [
    "--headless=new",
    `--remote-debugging-port=${DEBUG_PORT}`,
    `--user-data-dir=${USER_DATA_DIR}`,
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-extensions",
    "--hide-scrollbars",
    "--force-device-scale-factor=2",
    "about:blank",
  ], { stdio: "ignore", detached: false })
  for (let i = 0; i < 100; i += 1) {
    try {
      const v = await chromeVersion()
      console.log(`[chrome] launched: ${v.Browser}`)
      return child
    } catch {
      await sleep(200)
    }
  }
  throw new Error("Chrome did not expose its debugging port")
}

// ─── CDP over one browser socket, flattened sessions ────────────────────────

class CDP {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    this.waiters = []
    ws.addEventListener("message", (event) => {
      const msg = JSON.parse(event.data)
      if (msg.id !== undefined) {
        const entry = this.pending.get(msg.id)
        if (!entry) return
        this.pending.delete(msg.id)
        if (msg.error) entry.reject(new Error(`${entry.method}: ${msg.error.message}`))
        else entry.resolve(msg.result)
        return
      }
      for (const waiter of [...this.waiters]) {
        if (waiter.method === msg.method) {
          this.waiters.splice(this.waiters.indexOf(waiter), 1)
          waiter.resolve(msg.params)
        }
      }
    })
  }

  static async connect(url) {
    const ws = new WebSocket(url)
    await new Promise((resolve, reject) => {
      ws.addEventListener("open", resolve, { once: true })
      ws.addEventListener("error", reject, { once: true })
    })
    return new CDP(ws)
  }

  send(method, params = {}, sessionId) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, method })
      this.ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }))
    })
  }

  once(method, timeoutMs = 20000) {
    return new Promise((resolve) => {
      const waiter = { method, resolve }
      this.waiters.push(waiter)
      setTimeout(() => {
        const i = this.waiters.indexOf(waiter)
        if (i >= 0) {
          this.waiters.splice(i, 1)
          resolve(null)
        }
      }, timeoutMs)
    })
  }
}

// ─── page helpers ────────────────────────────────────────────────────────────

class Page {
  constructor(cdp, sessionId) {
    this.cdp = cdp
    this.sid = sessionId
  }

  send(method, params) {
    return this.cdp.send(method, params, this.sid)
  }

  async evaluate(expression) {
    const res = await this.send("Runtime.evaluate", {
      expression,
      returnByValue: true,
      awaitPromise: true,
    })
    if (res.exceptionDetails) {
      throw new Error(`evaluate failed: ${res.exceptionDetails.text} ${res.exceptionDetails.exception?.description ?? ""}`)
    }
    return res.result.value
  }

  async setViewport({ width, height, mobile = false, scale = 2 }) {
    await this.send("Emulation.setDeviceMetricsOverride", {
      width,
      height,
      deviceScaleFactor: scale,
      mobile,
      screenWidth: width,
      screenHeight: height,
    })
  }

  async navigate(url) {
    const loaded = this.cdp.once("Page.loadEventFired")
    await this.send("Page.navigate", { url })
    await loaded
  }

  async capture(name) {
    await this.evaluate(`document.querySelectorAll('nextjs-portal').forEach((n) => n.remove()); true`)
    const { data } = await this.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false })
    const file = join(OUT_DIR, `${name}.png`)
    writeFileSync(file, Buffer.from(data, "base64"))
    const bytes = Buffer.from(data, "base64").length
    console.log(`[shot] ${file} (${(bytes / 1024).toFixed(0)} KB)`)
    return file
  }
}

// ─── the actual run ──────────────────────────────────────────────────────────

async function resolveClawId() {
  const res = await fetch(`${HUB_URL}/api/claws`, {
    headers: { Authorization: `Bearer ${HUB_TOKEN}` },
  })
  if (!res.ok) throw new Error(`GET /api/claws → ${res.status} ${await res.text()}`)
  const claws = await res.json()
  const match = claws.find((c) => c.name.includes(CLAW_MATCH))
  if (!match) throw new Error(`no claw name contains ${JSON.stringify(CLAW_MATCH)}; have: ${claws.map((c) => c.name).join(", ")}`)
  console.log(`[hub] target claw: ${match.name} (${match.id}) status=${match.status}`)
  return match.id
}

/** Dispatch the full pointer sequence Radix/React needs on the sidebar row. */
const CLICK_SIDEBAR = (needle) => `(() => {
  const nodes = [...document.querySelectorAll('span,div,p,button')]
    .filter((n) => n.children.length === 0 && n.textContent && n.textContent.includes(${JSON.stringify(needle)}))
  if (nodes.length === 0) return 'no-match'
  const target = nodes[0].closest('button,[role=button],div')
  if (!target) return 'no-clickable-ancestor'
  for (const type of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click']) {
    target.dispatchEvent(new MouseEvent(type, { bubbles: true, cancelable: true, view: window }))
  }
  return 'clicked'
})()`

const SET_THEME = (mode) => `(() => {
  localStorage.setItem('theme', ${JSON.stringify(mode)})
  document.documentElement.classList.toggle('dark', ${mode === "dark"})
  document.documentElement.style.colorScheme = ${JSON.stringify(mode)}
  return document.documentElement.className
})()`

/**
 * Turn cards collapse by default, which hides every StepRow behind a one-line
 * header — useless for a visual diff of the step rows themselves. Expand every
 * collapsed turn (chevron without rotate-90) and every "Show N earlier tool
 * calls" pager, repeatedly, since expanding reveals more of both.
 */
const EXPAND_ALL = `(() => {
  let clicks = 0
  for (let pass = 0; pass < 6; pass += 1) {
    const before = clicks
    for (const button of document.querySelectorAll('section > button, section > div > button')) {
      if (button.getAttribute('aria-expanded') === 'true') continue
      const chevron = button.querySelector('svg')
      if (!chevron || chevron.classList.contains('rotate-90')) continue
      if (!/\\d+\\s+(steps?|tool calls?)/.test(button.textContent || '')) continue
      button.click()
      clicks += 1
    }
    for (const button of document.querySelectorAll('button')) {
      if (/Show \\d+ earlier tool calls?/.test(button.textContent || '')) {
        button.click()
        clicks += 1
      }
    }
    if (clicks === before) break
  }
  return clicks
})()`

const CONVERSATION_STATE = `(() => {
  const text = document.body.innerText || ''
  return {
    onLogin: location.pathname.includes('/login'),
    chars: text.length,
    hasUser: text.includes('token refresh test'),
    hasMarkdown: text.includes('Fixed — the cache'),
    hasTable: document.querySelectorAll('table').length,
    hasCode: document.querySelectorAll('pre').length,
    steps: document.body.innerText.split('\\n').filter((l) => /^(Read|Bash|Edit|Write|Grep|WebFetch|Task)\\b/.test(l.trim())).length,
  }
})()`

async function waitForConversation(page, { timeoutMs = 45000 } = {}) {
  const deadline = Date.now() + timeoutMs
  let last = null
  while (Date.now() < deadline) {
    last = await page.evaluate(CONVERSATION_STATE)
    if (last && !last.onLogin && last.hasMarkdown && last.hasCode > 0) return last
    await sleep(700)
  }
  return last
}

async function main() {
  mkdirSync(OUT_DIR, { recursive: true })
  const clawId = await resolveClawId()
  const chrome = await launchChrome()

  const { webSocketDebuggerUrl } = await chromeVersion()
  const cdp = await CDP.connect(webSocketDebuggerUrl)

  const { targetId } = await cdp.send("Target.createTarget", { url: "about:blank" })
  const { sessionId } = await cdp.send("Target.attachToTarget", { targetId, flatten: true })
  const page = new Page(cdp, sessionId)

  await page.send("Page.enable")
  await page.send("Runtime.enable")
  await page.setViewport({ width: 1440, height: 900 })

  // Seed auth + selection on the app origin *before* the app mounts:
  // useSyncExternalStore reads localStorage during the first client render, so
  // writing it afterwards would not select the claw without a click.
  await page.navigate(APP_URL)
  await page.evaluate(`(() => {
    sessionStorage.setItem('ec_hub_token', ${JSON.stringify(HUB_TOKEN)})
    localStorage.setItem('elasticclaw_selected_claw', ${JSON.stringify(clawId)})
    return true
  })()`)
  await page.navigate(APP_URL)
  await sleep(1500)

  // Belt and braces: if the board is showing instead of the chat, click the row.
  const state0 = await page.evaluate(CONVERSATION_STATE)
  if (!state0.hasMarkdown) {
    const clicked = await page.evaluate(CLICK_SIDEBAR(CLAW_MATCH))
    console.log(`[click] sidebar row: ${clicked}`)
  }

  const state = await waitForConversation(page)
  console.log(`[state] ${JSON.stringify(state)}`)
  if (!state || state.onLogin) throw new Error("landed on /login — the hub token was not accepted")
  if (!state.hasMarkdown) throw new Error("rich conversation never rendered; is the hub seeded and the claw selected?")

  const shots = []

  await page.setViewport({ width: 1440, height: 900 })
  await page.evaluate(SET_THEME("dark"))
  await sleep(500)
  shots.push(await page.capture(`${LABEL}-desktop-dark`))

  await page.evaluate(SET_THEME("light"))
  await sleep(500)
  shots.push(await page.capture(`${LABEL}-desktop-light`))

  await page.evaluate(SET_THEME("dark"))
  await page.setViewport({ width: 390, height: 844, mobile: true, scale: 3 })
  await sleep(1200)
  shots.push(await page.capture(`${LABEL}-mobile-dark`))

  // "Full page" for this app means the chat scroll container, not the document:
  // body is overflow-hidden and the transcript lives in its own scroller. A very
  // tall viewport is the only way to get the whole turn stack into one frame.
  await page.setViewport({ width: 1440, height: 3600, scale: 1 })
  await sleep(1500)
  console.log(`[expand] ${await page.evaluate(EXPAND_ALL)} turn cards / pagers opened`)
  await sleep(1200)
  await page.evaluate(`(() => {
    const scrollers = [...document.querySelectorAll('*')].filter((n) => {
      const s = getComputedStyle(n)
      return (s.overflowY === 'auto' || s.overflowY === 'scroll') && n.scrollHeight > n.clientHeight + 40
    })
    scrollers.sort((a, b) => b.scrollHeight - a.scrollHeight)
    if (scrollers[0]) scrollers[0].scrollTop = 0
    return scrollers[0] ? scrollers[0].scrollHeight : 0
  })()`)
  await sleep(800)
  // Grow the viewport until the transcript scroller no longer overflows, so
  // "full conversation" means the whole thing and not the first 3600px of it.
  for (let pass = 0; pass < 4; pass += 1) {
    const need = await page.evaluate(`(() => {
      const scrollers = [...document.querySelectorAll('*')].filter((n) => {
        const s = getComputedStyle(n)
        return (s.overflowY === 'auto' || s.overflowY === 'scroll') && n.scrollHeight > n.clientHeight + 40
      })
      scrollers.sort((a, b) => b.scrollHeight - a.scrollHeight)
      if (!scrollers[0]) return 0
      scrollers[0].scrollTop = 0
      return scrollers[0].scrollHeight - scrollers[0].clientHeight
    })()`)
    if (!need) break
    const height = Math.min(16000, 3600 + need + 200)
    await page.setViewport({ width: 1440, height, scale: 1 })
    await sleep(900)
    if (height >= 16000) break
  }
  shots.push(await page.capture(`${LABEL}-desktop-conversation-full`))

  if (chrome && !KEEP_CHROME) {
    await cdp.send("Browser.close").catch(() => {})
    chrome.kill()
  }
  cdp.ws.close()
  console.log(`\n[done] ${shots.length} screenshots in ${OUT_DIR}`)
}

main().catch((err) => {
  console.error(`[fail] ${err.stack || err.message}`)
  process.exit(1)
})
