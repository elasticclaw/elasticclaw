const assert = require("node:assert/strict")
const { readFileSync } = require("node:fs")
const path = require("node:path")
const test = require("node:test")
const ts = require("typescript")

global.window = { addEventListener() {}, removeEventListener() {} }

// Exercise the components' state/effect transitions without a browser or server.
function loadComponent(file, mocks) {
  const source = readFileSync(path.join(__dirname, "..", file), "utf8")
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  })
  const module = { exports: {} }
  new Function("require", "module", "exports", outputText)((id) => {
    if (Object.hasOwn(mocks, id)) return mocks[id]
    if (id === "react/jsx-runtime") return require(id)
    // Unused child components are outside these state-transition tests.
    if (id.startsWith("@/") || id.startsWith("./")) return {}
    throw new Error(`Unexpected import: ${id}`)
  }, module, module.exports)
  return module.exports
}

function createHooks() {
  const states = []
  const effects = []
  let cursor = 0
  let pending = []
  return {
    react: {
      useState(initial) {
        const index = cursor++
        if (!(index in states)) states[index] = initial
        return [states[index], (next) => {
          states[index] = typeof next === "function" ? next(states[index]) : next
        }]
      },
      useEffect(callback, deps) {
        const index = cursor++
        const previous = effects[index]
        if (!previous || deps.some((value, i) => !Object.is(value, previous.deps[i]))) {
          pending.push(() => {
            previous?.cleanup?.()
            effects[index] = { deps, callback, cleanup: callback() }
          })
        }
      },
      useRef(current) {
        const index = cursor++
        if (!(index in states)) states[index] = { current }
        return states[index]
      },
      useEffectEvent(callback) {
        const index = cursor++
        if (!(index in states)) {
          const cell = { callback }
          cell.invoke = (...args) => cell.callback(...args)
          states[index] = cell
        }
        states[index].callback = callback
        return states[index].invoke
      },
      Suspense: "Suspense",
      useCallback: (callback) => callback,
      useSyncExternalStore: (_subscribe, getSnapshot) => getSnapshot(),
    },
    restartEffects() {
      for (const effect of effects) effect?.cleanup?.()
      for (const effect of effects) if (effect) effect.cleanup = effect.callback()
    },
    render(component) {
      cursor = 0
      const tree = component()
      const callbacks = pending
      pending = []
      callbacks.forEach((callback) => callback())
      return tree
    },
  }
}

function findElement(tree, type) {
  if (!tree || typeof tree !== "object") return undefined
  if (tree.type === type) return tree
  const children = Array.isArray(tree) ? tree : [tree.props?.children]
  for (const child of children) {
    const found = findElement(child, type)
    if (found) return found
  }
}

function chatFixture(options = {}) {
  let query = options.thread ? `thread=${options.thread}` : ""
  const hooks = createHooks()
  const requests = []
  const api = {
    ApiError,
    fetchAIChatSources: (workspace, refresh) => new Promise((resolve) => requests.push({ workspace, refresh, resolve })),
    ...options.api,
  }
  const shared = {
    react: hooks.react,
    "next/navigation": { useRouter: () => ({ replace(url) { query = url.split("?")[1] ?? "" } }), useSearchParams: () => new URLSearchParams(query) },
    "@/lib/api": api,
  }
  // The screen's own hooks and helpers run for real, sharing the mocks.
  const local = (file, mocks = {}) => loadComponent(`components/ai-chat/${file}`, { ...shared, ...mocks })
  const { ChatScreen } = loadComponent("components/ai-chat/chat-screen.tsx", {
    ...shared,
    "lucide-react": { Sparkles: "Sparkles" },
    "@/hooks/use-feature-flag": { useFeatureFlag: () => true, useFeatureFlagsLoaded: () => true },
    "./use-conversation": local("use-conversation.ts", {
      "./use-chat-sources": local("use-chat-sources.ts"),
      "./stream-turn": local("stream-turn.ts"),
    }),
    "./header": { ChatHeader: "ChatHeader" },
    "./sources": local("sources.ts"),
    "./composer": { ChatComposer: "ChatComposer" },
    "./message-list": { MessageList: "MessageList" },
  })
  const component = hooks.render(ChatScreen).props.children.type
  return { requests, render: () => hooks.render(component), restartEffects: () => hooks.restartEffects(), navigate: (thread) => { query = thread ? `thread=${thread}` : "" } }
}

const sources = (workspace, workspaces, extra = {}) => ({
  workspace, workspaces, configured: true, sources: [], ...extra,
})

for (const remaining of [["A"], []]) {
  test(`refresh recovers after selected workspace is removed (${remaining.length} remaining)`, async () => {
    const { requests, render } = chatFixture()
    render()
    requests[0].resolve(sources("A", ["A", "B"]))
    await Promise.resolve()
    findElement(render(), "select").props.onChange({ target: { value: "B" } })
    render()
    assert.equal(requests[1].workspace, "B")
    requests[1].resolve(sources("B", ["A", "B"]))
    await Promise.resolve()
    findElement(render(), "ChatHeader").props.onRefreshSources()
    render()
    requests[2].resolve(sources("B", remaining, { configured: false }))
    await Promise.resolve()
    render()
    const next = remaining[0] ?? ""
    assert.equal(requests[3].workspace, next)
    requests[3].resolve(sources(next, remaining, { configured: remaining.length > 0 }))
    await Promise.resolve()
    const tree = render()
    assert.equal(findElement(tree, "ChatHeader").props.data.workspace, next)
    assert.equal(requests.length, 4)
    assert.equal(findElement(tree, "h2").props.children,
      remaining.length ? "What are we working on?" : "Not configured for this workspace")
  })
}

test("new chat shows only the examples and suggestions the workspace configures", async () => {
  for (const extra of [{}, { examples: { ask_the_data: "How many runs failed this week?" }, suggestions: ["Why do agents stall?"] }]) {
    const { requests, render } = chatFixture()
    render()
    requests[0].resolve(sources("A", ["A"], extra))
    await Promise.resolve()
    const tree = render()
    const text = elementText(tree)
    const cards = findCards(tree)
    assert.deepEqual(cards.props.examples, extra.examples ?? {})
    if (extra.suggestions) assert.match(text, /Suggested.*Why do agents stall\?/)
    else assert.doesNotMatch(text, /Suggested/)
  }
  const { ModeCards } = loadComponent("components/ai-chat/mode-cards.tsx", { "lucide-react": {}, "@/lib/utils": { cn: (...c) => c.filter(Boolean).join(" ") } })
  const cardsText = elementText(ModeCards({ mode: null, examples: { ask_the_data: "How many runs failed this week?" }, onSelect() {} }))
  assert.match(cardsText, /“How many runs failed this week\?”/)
  assert.equal(cardsText.match(/“/g).length, 1)
})

function findCards(tree) {
  if (!tree || typeof tree !== "object") return undefined
  if (tree.props?.examples !== undefined) return tree
  for (const child of [tree.props?.children].flat(Infinity)) {
    const found = findCards(child)
    if (found) return found
  }
}

test("invalid configuration renders the server error and keeps the workspace selected", async () => {
  const { requests, render } = chatFixture()
  render()
  requests[0].resolve(sources("A", ["A", "B"]))
  await Promise.resolve()
  findElement(render(), "select").props.onChange({ target: { value: "B" } })
  render()
  requests[1].resolve(sources("B", ["A", "B"], {
    configured: false, error: "retention days must be positive",
  }))
  await Promise.resolve()
  const tree = render()
  assert.equal(findElement(tree, "select").props.value, "B")
  assert.equal(findElement(tree, "h2").props.children, "Not configured for this workspace")
  assert.match(JSON.stringify(tree), /retention days must be positive/)
  assert.equal(requests.length, 2)
})

test("a stale sources request cannot change the current workspace", async () => {
  const { requests, render } = chatFixture()
  render()
  requests[0].resolve(sources("A", ["A", "B"]))
  await Promise.resolve()
  const selector = findElement(render(), "select")
  selector.props.onChange({ target: { value: "B" } })
  render()
  selector.props.onChange({ target: { value: "A" } })
  render()
  requests[2].resolve(sources("A", ["A", "B"]))
  await Promise.resolve()
  requests[1].resolve(sources("B", [], { configured: false }))
  await Promise.resolve()
  assert.equal(findElement(render(), "select").props.value, "A")
  assert.equal(requests.length, 3)
})

test("only a visible agent conversation is passed to useHub, preserving navigation selection", () => {
  const hooks = createHooks()
  const stopAtHub = new Error("captured hub selection")
  const selections = []
  let pathname = "/"
  const { HomeShell } = loadComponent("components/home-shell.tsx", {
    react: hooks.react,
    "next/navigation": { usePathname: () => pathname },
    "next/dynamic": { default: () => () => null },
    "@/hooks/use-mobile": { useIsMobile: () => false },
    "@/lib/shell-storage": {
      getSelectedClawId: () => "agent-B",
      getConfigured: () => true,
    },
    "@/hooks/use-hub": { useHub: (selection) => {
      selections.push(selection)
      throw stopAtHub
    } },
  })
  for (const route of ["/", "/chat", "/analytics", "/"]) {
    pathname = route
    assert.throws(() => hooks.render(HomeShell), (error) => error === stopAtHub)
  }
  assert.deepEqual(selections, ["agent-B", null, null, "agent-B"])
})

const flush = () => new Promise((resolve) => setImmediate(resolve))
class ApiError extends Error { constructor(message, status) { super(message); this.status = status } }
const thread = (id) => ({ id, workspace: "A", title: "Question", mode: "explore_idea", archivedAt: null })

test("existing thread reloads after StrictMode effect restart", async () => {
  const loads = []
  const fixture = chatFixture({ thread: "existing", api: {
    fetchAIChatThread: (id, signal) => new Promise((resolve) => loads.push({ id, signal, resolve })),
  } })
  fixture.render()
  fixture.restartEffects()
  assert.equal(loads.length, 2)
  assert.equal(loads[0].signal.aborted, true)
  loads[1].resolve({ thread: thread("existing"), messages: [] })
  await flush()
  assert.ok(findElement(fixture.render(), "MessageList"))
})

test("Back to new chat while an existing thread loads discards the stale load", async () => {
  const loads = []
  const fixture = chatFixture({ thread: "existing", api: {
    fetchAIChatThread: (_id, signal) => new Promise((resolve) => loads.push({ signal, resolve })),
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  fixture.navigate("")
  fixture.render()
  assert.equal(loads[0].signal.aborted, true)
  loads[0].resolve({ thread: thread("existing"), messages: [] })
  await flush()
  const tree = fixture.render()
  assert.equal(findElement(tree, "h2").props.children, "What are we working on?")
  assert.equal(findElement(tree, "MessageList"), undefined)
})

function captureWindowListeners() {
  const listeners = {}
  const previous = global.window
  global.window = { addEventListener(type, listener) { listeners[type] = listener }, removeEventListener() {} }
  return { emit: (type, detail) => listeners[type]({ detail }), restore: () => { global.window = previous } }
}

for (const syncFails of [false, true]) {
  test(`a slow initial thread load ${syncFails ? "still shows when the newer sync fails" : "cannot overwrite a newer cross-tab sync"}`, async () => {
    const windowEvents = captureWindowListeners()
    try {
      const loads = []
      const fixture = chatFixture({ thread: "existing", api: {
        fetchAIChatThread: (_id, signal) => new Promise((resolve, reject) => loads.push({ signal, resolve, reject })),
      } })
      fixture.render()
      windowEvents.emit("ai-chat-thread-updated", { threadId: "existing" })
    await new Promise((resolve) => setTimeout(resolve, 320))
      assert.equal(loads.length, 2)
      if (syncFails) loads[1].reject(new Error("offline"))
      else loads[1].resolve({ thread: thread("existing"), messages: [{ id: "a", role: "assistant", content: "Done", status: "completed" }] })
      await flush()
      loads[0].resolve({ thread: thread("existing"), messages: [{ id: "a", role: "assistant", content: "", status: "streaming" }] })
      await flush()
      const shown = findElement(fixture.render(), "MessageList").props.messages[0]
      assert.equal(shown.status, syncFails ? "streaming" : "completed")
    } finally {
      windowEvents.restore()
    }
  })
}

test("a late initial load failure cannot hide a newer cross-tab sync", async () => {
  const windowEvents = captureWindowListeners()
  try {
    const loads = []
    const fixture = chatFixture({ thread: "existing", api: {
      fetchAIChatThread: (_id, signal) => new Promise((resolve, reject) => loads.push({ signal, resolve, reject })),
    } })
    fixture.render()
    windowEvents.emit("ai-chat-thread-updated", { threadId: "existing" })
    await new Promise((resolve) => setTimeout(resolve, 320))
    loads[1].resolve({ thread: thread("existing"), messages: [{ id: "a", role: "assistant", content: "Done", status: "completed" }] })
    await flush()
    loads[0].reject(new Error("offline"))
    await flush()
    const tree = fixture.render()
    assert.equal(findElement(tree, "MessageList").props.messages[0].status, "completed")
    assert.equal(findElement(tree, "Loading conversation…"), undefined)
  } finally {
    windowEvents.restore()
  }
})

test("first send creates a thread, streams tokens, and ignores future event types", async () => {
  let emit
  let finish
  let created = 0
  const fixture = chatFixture({ api: {
    createAIChatThread: async (workspace, mode) => { created++; assert.equal(workspace, "A"); assert.equal(mode, "explore_idea"); return thread("new") },
    streamAIChatTurn: (id, text, retry, _signal, onEvent) => {
      assert.equal(id, "new"); assert.equal(text, "Question"); assert.equal(retry, false)
      emit = onEvent
      return new Promise((resolve) => { finish = resolve })
    },
    fetchAIChatThread: async () => ({ thread: thread("new"), messages: [{ id: "assistant", role: "assistant", content: "Hello", status: "completed" }] }),
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onChange("Question")
  findElement(fixture.render(), "ChatComposer").props.onSend()
  await flush()
  emit("message_started", { messageId: "assistant", model: "model" })
  emit("token", { text: "Hel" })
  emit("block", { type: "future_block", data: {} })
  emit("future_event", { text: "not a token" })
  emit("token", { text: "lo" })
  const tree = fixture.render()
  assert.equal(findElement(tree, "MessageList").props.messages[1].content, "Hello")
  assert.equal(findElement(tree, "ChatComposer").props.streaming, true)
  emit("done", { messageId: "assistant", status: "completed" })
  finish()
  await flush()
  assert.equal(created, 1)
  assert.equal(findElement(fixture.render(), "ChatComposer").props.streaming, false)
})

test("composer Enter sends, Shift+Enter and IME composition do not", () => {
  const { ChatComposer } = loadComponent("components/ai-chat/composer.tsx", { "lucide-react": {} })
  let sent = 0
  const tree = ChatComposer({ value: "Question", onChange() {}, onSend() { sent++ }, onCancel() {}, streaming: false })
  const key = findElement(tree, "textarea").props.onKeyDown
  for (const [shiftKey, isComposing] of [[true, false], [false, true], [false, false]]) key({ key: "Enter", shiftKey, nativeEvent: { isComposing }, preventDefault() {} })
  assert.equal(sent, 1)
})

const { readAIChatEvents } = loadComponent("lib/ai-chat-stream.ts", {})
function byteStream(text, size = 1) {
  const bytes = new TextEncoder().encode(text)
  return new ReadableStream({ start(controller) {
    for (let i = 0; i < bytes.length; i += size) controller.enqueue(bytes.slice(i, i + size))
    controller.close()
  } })
}

test("SSE handles split UTF-8, CRLF and multiline frames", async () => {
  const events = []
  await readAIChatEvents(byteStream('event: token\r\ndata: {"text":\r\ndata: "Olá 界"}\r\n\r\nevent: done\ndata: {"status":"completed"}\n\n'), (event, data) => events.push([event, data]))
  assert.deepEqual(events, [["token", { text: "Olá 界" }], ["done", { status: "completed" }]])
})

test("SSE rejects a disconnected stream without done", async () => {
  await assert.rejects(readAIChatEvents(byteStream('event: token\ndata: {"text":"partial"}\n\n'), () => {}), /connection ended/)
})

test("cancel aborts the active request without a delayed cancel that could affect a new turn", async () => {
  let signal
  let emit
  let cancelCalls = 0
  const fixture = chatFixture({ api: {
    createAIChatThread: async () => thread("new"),
    streamAIChatTurn: (_id, _text, _retry, requestSignal, onEvent) => {
      signal = requestSignal
      emit = onEvent
      return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new Error("aborted"))))
    },
    cancelAIChatTurn: async () => { cancelCalls++ },
    fetchAIChatThread: async () => ({ thread: thread("new"), messages: [{ id: "assistant", role: "assistant", content: "Partial", status: "cancelled" }] }),
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onChange("Question")
  findElement(fixture.render(), "ChatComposer").props.onSend()
  await flush()
  emit("message_started", { messageId: "assistant" })
  findElement(fixture.render(), "ChatComposer").props.onCancel()
  await flush()
  assert.equal(signal.aborted, true)
  assert.equal(cancelCalls, 0)
  const tree = fixture.render()
  assert.equal(findElement(tree, "ChatComposer").props.streaming, false)
  assert.equal(findElement(tree, "MessageList").props.messages[0].status, "cancelled")
})

test("lost message_started reconciles the persisted turn before retrying", async () => {
  const attempts = []
  const fixture = chatFixture({ api: {
    createAIChatThread: async () => thread("new"),
    streamAIChatTurn: async (_id, text, retry) => { attempts.push({ text, retry }); throw new Error("connection lost") },
    fetchAIChatThread: async () => ({ thread: thread("new"), messages: [{ id: "persisted", role: "assistant", content: "", status: "cancelled" }] }),
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onChange("Question")
  findElement(fixture.render(), "ChatComposer").props.onSend()
  await flush()
  const alert = findElement(fixture.render(), "section").props.children.find((child) => child?.props?.role === "alert")
  alert.props.children[1].props.onClick()
  await flush()
  assert.deepEqual(attempts, [{ text: "Question", retry: false }, { text: "Question", retry: true }])
})

test("a message the hub rejected is resent, never retried as another tab's failed turn", async () => {
  const windowEvents = captureWindowListeners()
  try {
    const attempts = []
    let saved = { thread: thread("existing"), messages: [{ id: "a1", role: "assistant", content: "Earlier", status: "completed" }] }
    const fixture = chatFixture({ thread: "existing", api: {
      fetchAIChatThread: async () => saved,
      streamAIChatTurn: async (_id, text, retry) => { attempts.push({ text, retry }); throw new ApiError("Too many turns in progress.", 429) },
    } })
    fixture.render()
    fixture.requests[0].resolve(sources("A", ["A"]))
    await flush()
    findElement(fixture.render(), "ChatComposer").props.onChange("New question")
    findElement(fixture.render(), "ChatComposer").props.onSend()
    await flush()
    // Another tab's turn on this thread fails afterwards.
    saved = { thread: thread("existing"), messages: [...saved.messages, { id: "a2", role: "assistant", content: "", status: "cancelled" }] }
    windowEvents.emit("ai-chat-thread-updated", { threadId: "existing" })
    await new Promise((resolve) => setTimeout(resolve, 320))
    await flush()
    const alert = findElement(fixture.render(), "section").props.children.find((child) => child?.props?.role === "alert")
    alert.props.children[1].props.onClick()
    await flush()
    assert.deepEqual(attempts, [{ text: "New question", retry: false }, { text: "New question", retry: false }])
  } finally {
    windowEvents.restore()
  }
})

test("a new thread pins the workspace the server picked for its sources", async () => {
  const fixture = chatFixture({ api: {
    createAIChatThread: async () => thread("new"),
    streamAIChatTurn: async () => {},
    fetchAIChatThread: async () => ({ thread: thread("new"), messages: [] }),
  } })
  fixture.render()
  assert.equal(fixture.requests[0].workspace, "")
  fixture.requests[0].resolve(sources("A", ["A", "B"]))
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onChange("Question")
  findElement(fixture.render(), "ChatComposer").props.onSend()
  await flush()
  const tree = fixture.render()
  assert.equal(fixture.requests.at(-1).workspace, "A")
  // The default load already answers for A, so sources stay visible meanwhile.
  assert.equal(findElement(tree, "ChatHeader").props.data.workspace, "A")
  findElement(tree, "ChatHeader").props.onRefreshSources()
  fixture.render()
  assert.equal(fixture.requests.at(-1).workspace, "A")
})

test("retrying a failed reply keeps a new question typed in the composer", async () => {
  const fixture = chatFixture({ thread: "existing", api: {
    fetchAIChatThread: async () => ({ thread: thread("existing"), messages: [{ id: "a1", role: "assistant", content: "", status: "error" }] }),
    streamAIChatTurn: async (_id, _text, retry, _signal, onEvent) => {
      assert.equal(retry, true)
      onEvent("message_started", { messageId: "a2", model: "m" })
      onEvent("done", { messageId: "a2", status: "completed" })
    },
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onChange("Next question")
  const alert = findElement(fixture.render(), "section").props.children.find((child) => child?.props?.role === "alert")
  alert.props.children[1].props.onClick()
  await flush()
  assert.equal(findElement(fixture.render(), "ChatComposer").props.value, "Next question")
})

const sourceHelpers = loadComponent("components/ai-chat/sources.ts", {})
const source = (kind, status = "connected", extra = {}) => ({ kind, name: kind, status, access: "read", ...extra })

test("source statuses distinguish healthy, unreachable, invalid, and future values", () => {
  const { sourceStatus, sourceSummary } = sourceHelpers
  assert.equal(sourceStatus("connected").label, "Connected")
  assert.equal(sourceStatus("unreachable").label, "Can't reach")
  assert.equal(sourceStatus("unreachable").warning, true)
  assert.match(sourceStatus("unreachable").textClass, /amber/)
  assert.equal(sourceStatus("invalid").label, "Invalid")
  assert.equal(sourceStatus("invalid").textClass, "text-destructive")
  assert.equal(sourceStatus("future-status").label, "Unknown")
  const healthy = [source("posthog"), source("datadog")]
  assert.deepEqual(sourceSummary(healthy), { label: "2 sources", dotClass: "bg-green-500" })
  for (const status of ["unreachable", "invalid"]) {
    assert.deepEqual(sourceSummary([healthy[0], source("datadog", status)]), { label: "1 of 2 sources", dotClass: "bg-amber-500" })
  }
  assert.equal(sourceSummary(healthy, true).dotClass, "bg-muted-foreground")
  assert.equal(sourceSummary([]).dotClass, "bg-muted-foreground")
})

test("source groups follow the design order and preserve unknown providers", () => {
  const groups = sourceHelpers.groupSources([source("issue_tracker"), source("future"), source("datadog"), source("repositories"), source("posthog"), source("knowledge_base")])
  assert.deepEqual(groups.map((group) => group.name), ["Knowledge", "Data", "Tracker", "Other"])
  assert.deepEqual(groups[0].sources.map((item) => item.kind), ["knowledge_base", "repositories"])
  assert.deepEqual(groups[1].sources.map((item) => item.kind), ["posthog", "datadog"])
  assert.equal(groups[3].sources[0].kind, "future")
  assert.deepEqual(sourceHelpers.groupSources([]), [])
})

function elementText(tree) {
  if (Array.isArray(tree)) return tree.map(elementText).join("")
  if (tree == null || typeof tree === "boolean") return ""
  if (typeof tree !== "object") return String(tree)
  return elementText(tree.props?.children)
}
// Interview controls are wrapped in a Choice component, so match them by input type too.
function findInputs(tree) {
  if (!tree || typeof tree !== "object") return []
  if (Array.isArray(tree)) return tree.flatMap(findInputs)
  const own = tree.type === "input" || (typeof tree.type === "function" && ["radio", "checkbox"].includes(tree.props?.type)) ? [tree] : []
  return [...own, ...findInputs(tree.props?.children)]
}
function findElements(tree, type) {
  if (!tree || typeof tree !== "object") return []
  if (Array.isArray(tree)) return tree.flatMap((item) => findElements(item, type))
  return [...(tree.type === type ? [tree] : []), ...findElements(tree.props?.children, type)]
}

test("sources popover renders grouped details, read badges, errors, workspace, and refresh", () => {
  const { ChatHeader } = loadComponent("components/ai-chat/header.tsx", {
    "lucide-react": Object.fromEntries(["Activity", "BarChart3", "Book", "ChevronDown", "Database", "GitBranch", "Plus", "RefreshCw", "Ticket", "TriangleAlert"].map((name) => [name, name])),
    "@/components/ui/button": { Button: "Button" },
    "@/components/ui/popover": { Popover: "Popover", PopoverContent: "PopoverContent", PopoverTrigger: "PopoverTrigger" },
    "./mode-cards": { chatModes: [] },
    "./sources": sourceHelpers,
  })
  let refreshed = 0
  const props = { mode: null, onNewChat() {}, onRefreshSources() { refreshed++ }, data: sources("fasterway", ["fasterway"], { sources: [
    source("knowledge_base", "connected", { name: "Knowledge base", detail: "owner/repo · 61 pages" }),
    source("repositories", "connected", { name: "Repositories", detail: "acme/web, acme/api, other/tools · default branch" }),
    source("posthog", "connected", { name: "PostHog", detail: "Project Web app · events and saved insights" }),
    source("datadog", "unreachable", { name: "Datadog", error: "The API key was rejected." }),
    source("issue_tracker", "invalid", { name: "Linear", error: "Choose a team" }),
    source("__proto__", "future-status", { name: "Future provider" }),
  ] }) }
  const tree = ChatHeader(props)
  const text = elementText(tree)
  for (const expected of ["What this chat can read", "Workspace fasterway", "The assistant reads these and never changes them.", "owner/repo · 61 pages", "3 of 6 sources", "Connected", "Can't reach", "The API key was rejected. Answers will skip Datadog until a check succeeds.", "Invalid", "Choose a team. Answers will skip Linear until it is fixed in the workspace configuration.", "web, api, tools", "Future provider", "Unknown", "Set in the workspace configuration, the same for everyone in this workspace."]) assert.ok(text.includes(expected), expected)
  assert.doesNotMatch(text, /create artifacts|create ticket|unchecked/i)
  assert.equal(findElements(tree, "span").filter((item) => elementText(item) === "Read").length, 6)
  assert.equal(findElements(tree, "h3").map(elementText).join(","), "Knowledge,Data,Tracker,Other")
  for (const name of ["web", "api", "tools"]) assert.ok(findElements(tree, "span").some((item) => elementText(item) === name && item.props.className === "font-mono"), name)
  assert.ok(findElements(tree, "span").some((item) => elementText(item) === "owner/repo" && item.props.className === "font-mono"))
  assert.ok(findElement(tree, "Database"))
  const refresh = findElements(tree, "Button").find((item) => item.props["aria-label"] === "Refresh sources")
  refresh.props.onClick()
  assert.equal(refreshed, 1)
  const loading = ChatHeader({ ...props, loading: true })
  assert.equal(findElements(loading, "Button").find((item) => item.props["aria-label"] === "Refresh sources").props.disabled, true)
  assert.equal(findElement(loading, "Button").props["aria-busy"], true)
})

test("refresh requests live health and keeps source details visible while loading", async () => {
  const fixture = chatFixture()
  fixture.render()
  assert.equal(fixture.requests[0].refresh, false)
  fixture.requests[0].resolve(sources("A", ["A", "B"], { sources: [source("posthog")] }))
  await flush()
  findElement(fixture.render(), "ChatHeader").props.onRefreshSources()
  const refreshing = findElement(fixture.render(), "ChatHeader")
  assert.equal(fixture.requests[1].refresh, true)
  assert.equal(refreshing.props.loading, true)
  assert.equal(refreshing.props.data.sources[0].kind, "posthog")
  fixture.requests[1].resolve(sources("A", ["A", "B"]))
  await flush()
  findElement(fixture.render(), "select").props.onChange({ target: { value: "B" } })
  const switched = findElement(fixture.render(), "ChatHeader")
  assert.equal(fixture.requests[2].refresh, false)
  assert.equal(switched.props.data, undefined)
})

test("sources fetch encodes workspace and sends refresh=1 only when requested", async () => {
  const { fetchAIChatSources } = loadComponent("lib/api.ts", {
    "./hub-url": { getHubUrl: () => "" },
    "./auth-storage": { getAuthToken: () => "test-token" },
  })
  const previous = global.fetch
  const urls = []
  global.fetch = async (url) => { urls.push(url); return { ok: true, status: 200, json: async () => sources("A", ["A"]) } }
  try {
    await fetchAIChatSources()
    await fetchAIChatSources("A & B", true)
    await fetchAIChatSources("A")
    assert.deepEqual(urls, ["/api/ai-chat/sources", "/api/ai-chat/sources?workspace=A+%26+B&refresh=1", "/api/ai-chat/sources?workspace=A"])
  } finally { global.fetch = previous }
})

test("tool progress uses the provider display name and clears on completion", () => {
  const { applyStreamEvent } = loadComponent("components/ai-chat/stream-turn.ts", {})
  const updates = []
  const view = { setReading: (value) => updates.push(value) }
  for (const provider of ["Knowledge base", "Repositories", "PostHog", "Datadog", "Linear"]) {
    applyStreamEvent({}, "tool_started", { tool: "internal_tool", provider }, view)
    applyStreamEvent({}, "tool_finished", { provider }, view)
  }
  assert.deepEqual(updates, ["Knowledge base", "", "Repositories", "", "PostHog", "", "Datadog", "", "Linear", ""])
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
  })
  const tree = hooks.render(() => MessageList({ messages: [], reading: "Knowledge base" }))
  assert.equal(elementText(findElement(tree, "p")), "Reading Knowledge base…")
})

test("SSE heartbeat comments do not appear as events", async () => {
  const events = []
  await readAIChatEvents(byteStream(': ping\n\nevent: token\ndata: {"text":"Hi"}\n\n: ping\n\nevent: done\ndata: {"status":"completed"}\n\n'), (event, data) => events.push([event, data]))
  assert.deepEqual(events, [["token", { text: "Hi" }], ["done", { status: "completed" }]])
})

const blockHelpers = loadComponent("components/ai-chat/blocks.tsx", {
  "lucide-react": Object.fromEntries(["CircleCheck", "FileCode2", "Info", "Ticket", "TriangleAlert"].map((name) => [name, name])),
  "@/components/ui/button": { Button: "Button" },
})
const interviewFixture = () => ({
  questionSetId: "set-1", title: "Framing the problem", answered: false,
  steps: [
    { skippable: true, questions: [
      { id: "q1", prompt: "When?", kind: "single", options: [{ label: "Planning", detail: "Before shopping" }, { label: "Shopping" }], other: true },
      { id: "q2", prompt: "Who?", label: "Who", kind: "multi", options: [{ label: "Clients" }, { label: "Coaches" }], other: true },
    ] },
    { skippable: true, questions: [{ id: "q3", prompt: "Where?", kind: "single", options: [{ label: "Home" }, { label: "Away" }], other: true }] },
  ],
})
const answersFixture = () => ({ questionSetId: "set-1", steps: [
  { skipped: false, answers: [{ questionId: "q1", choices: ["Shopping"] }, { questionId: "q2", choices: ["Clients", "Coaches"], other: " Friends " }] },
  { skipped: true, answers: [] },
] })

test("blocks split content at Unicode code points, preserve order, and ignore future blocks", () => {
  const a = { id: "a", type: "callout", at: 2, data: { tone: "info", text: "A" } }
  const b = { ...a, id: "b", type: "table", data: { columns: ["C"], rows: [["R"]] } }
  const c = { ...a, id: "c", at: 4 }
  assert.deepEqual(blockHelpers.splitContent("A😀界Z", [a, b, { type: "future", at: 3 }, c]), ["A😀", a, b, "界Z", c])
  assert.deepEqual(blockHelpers.splitContent("hello"), ["hello"])
  assert.deepEqual(blockHelpers.splitContent("", [a]), [a])
})

test("wrap-up is absent without an action handler and dispatches only with a handler", () => {
  const block = { id: "a", at: 0, type: "actions", actions: ["artifact", "ticket"] }
  assert.equal(blockHelpers.InlineBlock({ block }), null)
  const actions = []
  const tree = blockHelpers.InlineBlock({ block, onAction: (action) => actions.push(action) })
  assert.match(elementText(tree), /Wrap up.*Create HTML artifact/)
  findElements(tree, "Button").forEach((button) => button.props.onClick())
  assert.deepEqual(actions, ["artifact"])
  assert.ok(!elementText(tree).includes("Open Linear ticket"))
})

test("stat tiles, callouts and tables render model data as plain text", () => {
  const html = "<img src=x onerror=alert(1)> **bold**"
  for (const block of [
    { type: "stat_tiles", data: { tiles: [{ label: html, value: "42", caption: "PostHog" }] } },
    { type: "callout", data: { tone: "warning", title: "Check", text: html } },
    { type: "table", data: { columns: ["Column"], rows: [[html]] } },
  ]) {
    const tree = blockHelpers.InlineBlock({ block })
    assert.ok(elementText(tree).includes(html))
    assert.equal(findElements(tree, "img").length, 0)
  }
})

test("stream events retain blocks and interview, and answers create a summary instead of a text bubble", () => {
  const { applyStreamEvent, summarizeInterview } = loadComponent("components/ai-chat/stream-turn.ts", {})
  const interview = interviewFixture()
  const answers = answersFixture()
  let messages = [{ id: "old", role: "assistant", interview }]
  let draft = "unrelated draft"
  const turn = { threadId: "t", text: "", retry: false, answers, answersSummary: summarizeInterview(interview, answers), started: false, assistantId: "", error: "" }
  const view = { setMessages: (fn) => { messages = fn(messages) }, setDraft: (fn) => { draft = fn(draft) }, setReading() {} }
  applyStreamEvent(turn, "message_started", { messageId: "new" }, view)
  const block = { id: "block", type: "callout", at: 1, data: { tone: "info", text: "Shown" } }
  applyStreamEvent(turn, "token", { text: "😀" }, view)
  applyStreamEvent(turn, "block", block, view)
  applyStreamEvent(turn, "block", { id: "future", type: "future", at: 1 }, view)
  applyStreamEvent(turn, "interview", { ...interview, step: 1, total: 2 }, view)
  assert.equal(messages[0].interview.answered, true)
  assert.equal(messages[1].interview.answers[1].answer, "Clients, Coaches, Friends")
  assert.equal(messages[1].content, "")
  assert.equal(draft, "unrelated draft")
  assert.deepEqual(messages[2].blocks, [block])
  assert.deepEqual(messages[2].interview, interview)
})

test("interview single, multi and other choices follow their selection rules", () => {
  const helpers = loadComponent("components/ai-chat/interview-card.tsx", { react: {}, "lucide-react": {} })
  const [single, multi] = interviewFixture().steps[0].questions
  let answer = { questionId: single.id, choices: [] }
  answer = helpers.selectChoice(single, answer, "Planning")
  answer = helpers.selectChoice(single, answer, "Shopping")
  assert.deepEqual(answer.choices, ["Shopping"])
  answer = helpers.enterOther(single, answer, "At dinner")
  assert.deepEqual(answer.choices, [])
  answer = helpers.selectChoice(single, answer, "Planning")
  assert.equal(answer.other, undefined)
  let many = { questionId: multi.id, choices: [] }
  many = helpers.selectChoice(multi, many, "Clients")
  many = helpers.selectChoice(multi, many, "Coaches")
  many = helpers.enterOther(multi, many, "Friends")
  assert.deepEqual(many.choices, ["Clients", "Coaches"])
  many = helpers.selectChoice(multi, many, "Clients")
  assert.deepEqual(many.choices, ["Coaches"])
  assert.equal(many.other, "Friends")
  assert.equal(helpers.stepComplete([single, multi], new Map([["q1", answer]])), false)
  assert.equal(helpers.stepComplete([single, multi], new Map([["q1", answer], ["q2", many]])), true)
  assert.equal(helpers.stepComplete([single], new Map([["q1", { choices: [], other: "  " }]])), false)
})

test("interview card enables Continue only with answers, walks steps, and allows Skip", () => {
  const hooks = createHooks()
  const { InterviewCard } = loadComponent("components/ai-chat/interview-card.tsx", {
    react: hooks.react, "lucide-react": { ArrowRight: "ArrowRight" }, "@/components/ui/button": { Button: "Button" },
  })
  const submissions = []
  const props = { interview: interviewFixture(), disabled: false, onAnswer: (...args) => submissions.push(args) }
  const render = () => hooks.render(() => InterviewCard(props))
  const button = (tree, label) => findElements(tree, "Button").find((item) => elementText(item) === label)
  let tree = render()
  assert.equal(button(tree, "Continue").props.disabled, true)
  const fields = findElements(tree, "fieldset")
  findInputs(fields[0]).find((input) => input.props.type === "radio").props.onChange()
  tree = render()
  assert.equal(button(tree, "Continue").props.disabled, true)
  const other = findElements(findElements(tree, "fieldset")[1], "input").find((input) => input.props.type === "text")
  other.props.onChange({ target: { value: "Friends" } })
  tree = render()
  assert.equal(button(tree, "Continue").props.disabled, false)
  button(tree, "Continue").props.onClick()
  tree = render()
  assert.match(elementText(tree), /Step 2 of 2/)
  assert.equal(submissions.length, 0)
  button(tree, "Skip").props.onClick()
  assert.equal(submissions.length, 1)
  assert.equal(submissions[0][0], "set-1")
  assert.equal(submissions[0][1][0].answers[1].other, "Friends")
  assert.deepEqual(submissions[0][1][1], { skipped: true, answers: [] })
  props.disabled = true
  tree = render()
  assert.ok(findElements(tree, "fieldset").every((field) => field.props.disabled))
  assert.equal(button(tree, "Skip").props.disabled, true)
})

test("answers summary renders question labels, answers and skipped steps", () => {
  const { summarizeInterview } = loadComponent("components/ai-chat/stream-turn.ts", {})
  const summary = summarizeInterview(interviewFixture(), answersFixture())
  const tree = blockHelpers.AnswersSummary({ summary })
  assert.equal(elementText(tree), "Your answersWhen?: Shopping · Who: Clients, Coaches, Friends · 1 step skipped")
  assert.deepEqual(findElements(tree, "strong").map(elementText), ["When?:", "Who:"])
})

for (const id of ["constructor", "__proto__", "toString"]) {
  test(`interview handles prototype question id ${id}`, () => {
    const hooks = createHooks()
    const { InterviewCard, stepComplete } = loadComponent("components/ai-chat/interview-card.tsx", {
      react: hooks.react, "lucide-react": { ArrowRight: "ArrowRight" }, "@/components/ui/button": { Button: "Button" },
    })
    const question = { ...interviewFixture().steps[0].questions[0], id }
    const interview = { ...interviewFixture(), steps: [{ skippable: true, questions: [question] }] }
    const submissions = []
    const render = () => hooks.render(() => InterviewCard({ interview, disabled: false, onAnswer: (...args) => submissions.push(args) }))
    const continueButton = (tree) => findElements(tree, "Button").find((button) => elementText(button) === "Continue")
    assert.equal(stepComplete([question], new Map()), false)
    let tree = render()
    assert.equal(continueButton(tree).props.disabled, true)
    assert.ok(findInputs(tree).filter((input) => input.props.type === "radio").every((input) => input.props.checked === false))
    findInputs(tree).find((input) => input.props.type === "text").props.onChange({ target: { value: "At dinner" } })
    tree = render()
    assert.equal(findInputs(tree).find((input) => input.props["aria-label"] === "Other: When?").props.checked, true)
    assert.equal(continueButton(tree).props.disabled, false)
    findInputs(tree).find((input) => input.props.type === "radio").props.onChange()
    tree = render()
    assert.equal(findInputs(tree).find((input) => input.props["aria-label"] === "Other: When?").props.checked, false)
    assert.equal(continueButton(tree).props.disabled, false)
    continueButton(tree).props.onClick()
    assert.deepEqual(submissions, [["set-1", [{ skipped: false, answers: [{ questionId: id, choices: ["Planning"] }] }]]])
  })
}

test("only the latest unanswered interview is interactive, and answered cards disappear", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock", AnswersSummary: "AnswersSummary" },
    "./interview-card": { InterviewCard: "InterviewCard" },
  })
  const base = { id: "a", role: "assistant", content: "", createdAt: 0, status: "completed", interview: interviewFixture() }
  const render = (messages, streaming = false) => hooks.render(() => MessageList({ messages, streaming, reading: "", onAnswer() {} }))
  assert.equal(findElement(render([base]), "InterviewCard").props.disabled, false)
  assert.equal(findElement(render([base], true), "InterviewCard").props.disabled, true)
  for (const status of ["streaming", "cancelled", "error", "limit"]) {
    assert.equal(findElement(render([{ ...base, status }]), "InterviewCard").props.disabled, true, status)
  }
  assert.equal(findElement(render([base, { ...base, id: "user", role: "user", interview: undefined }]), "InterviewCard").props.disabled, true)
  assert.equal(findElement(render([{ ...base, interview: { ...base.interview, answered: true } }]), "InterviewCard"), undefined)
  const summary = { questionSetId: "set-1", title: "Framing", answers: [], skipped: 2 }
  const tree = render([{ ...base, role: "user", interview: summary }])
  assert.ok(findElement(tree, "AnswersSummary"))
  assert.ok(!findElement(tree, "article").props.className.includes("w-fit"))
  const folded = render([{ ...base, role: "user", id: "answers", interview: summary }, { ...base, id: "reply", interview: undefined, content: "Three directions" }])
  const articles = findElements(folded, "article")
  assert.equal(articles.length, 1)
  assert.equal(findElement(articles[0], "AnswersSummary").props.summary, summary)
  assert.ok(elementText(articles[0]).includes("Assistant"))
})

for (const lostStart of [false, true]) {
  test(`interview submission retry preserves the right request (${lostStart ? "lost message_started" : "HTTP rejection"})`, async () => {
    const interview = interviewFixture()
    const initial = { id: "interview", role: "assistant", status: "completed", content: "", interview }
    const attempts = []
    const fixture = chatFixture({ thread: "t", api: {
      fetchAIChatThread: async () => ({ thread: thread("t"), messages: attempts.length && lostStart ? [initial, { id: "failed", role: "assistant", status: "error", content: "" }] : [initial] }),
      streamAIChatTurn: async (_id, text, retry, _signal, _onEvent, answers) => {
        attempts.push({ text, retry, answers })
        if (lostStart) throw new Error("connection lost")
        throw new ApiError("Too many turns", 429)
      },
    } })
    fixture.render()
    fixture.requests[0].resolve(sources("A", ["A"]))
    await flush()
    let tree = fixture.render()
    assert.equal(findElement(tree, "ChatComposer").props.placeholder, "Answer here instead, or ask something…")
    findElement(tree, "MessageList").props.onAnswer("set-1", answersFixture().steps)
    await flush()
    tree = fixture.render()
    const alert = findElement(tree, "section").props.children.find((child) => child?.props?.role === "alert")
    alert.props.children[1].props.onClick()
    await flush()
    assert.equal(attempts.length, 2)
    assert.deepEqual(attempts[0].answers, answersFixture())
    assert.equal(attempts[1].retry, lostStart)
    assert.deepEqual(attempts[1].answers, lostStart ? undefined : answersFixture())
  })
}

const artifactHelpers = loadComponent("lib/ai-chat-artifacts.ts", { "./hub-url": { getHubUrl: () => "" } })
const artifactCardMocks = {
  react: {},
  "lucide-react": Object.fromEntries(["Check", "Copy", "ExternalLink", "FileCode2", "Loader2", "RotateCcw", "TriangleAlert", "X"].map((name) => [name, name])),
  "@/components/ui/button": { Button: "Button" },
  "@/lib/ai-chat-artifacts": { artifactShareURL: (slug, url) => url || `https://hub.test/artifacts/${slug}` },
}

test("artifact cards render working, created and failed states with the right actions", () => {
  const { ArtifactActionCard } = loadComponent("components/ai-chat/artifact-card.tsx", artifactCardMocks)
  const card = { title: "Meal swap", brief: "Phone prototype", artifactId: "id", slug: "meal-swap-abcdef", version: 2, bytes: 38 * 1024, lines: 412 }
  let stopped = false
  let tree = ArtifactActionCard({ block: { type: "action", kind: "artifact", state: "working", card }, onStop: () => { stopped = true } })
  assert.match(elementText(tree), /Building “Meal swap”Writing HTML · 412 lines so farStop/)
  findElement(tree, "Button").props.onClick()
  assert.equal(stopped, true)
  tree = ArtifactActionCard({ block: { type: "action", kind: "artifact", state: "created", card } })
  assert.match(elementText(tree), /ARTIFACTv2 · 38 KBMeal swap/)
  assert.match(elementText(tree), /Ask for changes and I will publish a new version at the same link/)
  assert.equal(findElement(tree, "a").props.href, "https://hub.test/artifacts/meal-swap-abcdef")
  assert.equal(findElement(tree, "a").props.target, "_blank")
  assert.ok(elementText(tree).includes("hub.test/artifacts/meal-swap-abcdef") && !elementText(tree).includes("https://"))
  assert.ok(!elementText(tree).includes("Linear"))
  let retry
  tree = ArtifactActionCard({ block: { type: "action", kind: "artifact", state: "failed", card: { ...card, error: "Stopped. Nothing was published." } }, onRetry: (action) => { retry = action } })
  assert.match(elementText(tree), /Could not save the artifactStopped. Nothing was published.Try again/)
  findElement(tree, "Button").props.onClick()
  assert.deepEqual(retry, { kind: "artifact", artifactId: "id", title: "Meal swap", brief: "Phone prototype" })
  assert.equal(findElement(ArtifactActionCard({ block: { type: "action", kind: "artifact", state: "failed", card } }), "Button"), undefined)
  assert.equal(ArtifactActionCard({ block: { kind: "ticket", state: "working", card } }), null)
})

test("copy artifact link shows feedback", async () => {
  const hooks = createHooks()
  const { CopyArtifactLink } = loadComponent("components/ai-chat/artifact-card.tsx", { ...artifactCardMocks, react: hooks.react })
  const descriptor = Object.getOwnPropertyDescriptor(global, "navigator")
  let copied
  Object.defineProperty(global, "navigator", { configurable: true, value: { clipboard: { writeText: async (value) => { copied = value } } } })
  try {
    let tree = hooks.render(() => CopyArtifactLink({ url: "https://hub.test/artifacts/meal-abcdef" }))
    tree.props.onClick()
    await flush()
    tree = hooks.render(() => CopyArtifactLink({ url: "https://hub.test/artifacts/meal-abcdef" }))
    assert.equal(elementText(tree), "Copied")
    assert.equal(copied, "https://hub.test/artifacts/meal-abcdef")
  } finally { if (descriptor) Object.defineProperty(global, "navigator", descriptor); else delete global.navigator }
})

test("action events update one card by kind without creating a user message or clearing the draft", () => {
  const { applyStreamEvent } = loadComponent("components/ai-chat/stream-turn.ts", {})
  let messages = []
  let draft = "Keep this draft"
  const turn = { threadId: "t", text: "", retry: false, action: { kind: "artifact" }, assistantId: "" }
  const view = { setMessages: (fn) => { messages = fn(messages) }, setDraft: (fn) => { draft = fn(draft) }, setReading() {} }
  applyStreamEvent(turn, "message_started", { messageId: "m" }, view)
  for (const [state, lines] of [["working", 1], ["working", 50], ["created", 0]]) applyStreamEvent(turn, "action", { kind: "artifact", state, card: { title: "Meal", lines, slug: state === "created" ? "meal-abcdef" : undefined } }, view)
  applyStreamEvent(turn, "action", { kind: "ticket", state: "created", card: { title: "Ignored" } }, view)
  assert.equal(messages.length, 1)
  assert.equal(messages[0].role, "assistant")
  assert.equal(messages[0].blocks.length, 1)
  assert.equal(messages[0].blocks[0].state, "created")
  assert.equal(messages[0].blocks[0].id, "m-artifact")
  assert.equal(draft, "Keep this draft")
  assert.deepEqual(blockHelpers.splitContent("", messages[0].blocks), messages[0].blocks)
})

test("wrap-up is interactive only on the latest completed reply; failed cards retry only on the latest", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock" }, "./artifact-card": { ArtifactActionCard: "ArtifactActionCard" },
  })
  const base = { id: "a", role: "assistant", content: "", createdAt: 0, status: "completed", blocks: [{ id: "wrap", type: "actions", at: 0, actions: ["artifact"] }] }
  let action
  const render = (messages, streaming = false) => hooks.render(() => MessageList({ messages, streaming, reading: "", onArtifact: (value) => { action = value ?? "new" } }))
  findElement(render([base]), "InlineBlock").props.onAction("artifact")
  assert.equal(action, "new")
  for (const [messages, streaming] of [[[base], true], [[{ ...base, status: "streaming" }], false], [[base, { ...base, id: "next" }], false]]) {
    assert.equal(findElement(render(messages, streaming), "InlineBlock").props.onAction, undefined)
  }
  const failed = { ...base, blocks: [{ id: "failed", type: "action", kind: "artifact", state: "failed", card: { title: "Meal" }, at: 0 }] }
  assert.equal(typeof findElement(render([failed]), "ArtifactActionCard").props.onRetry, "function")
  assert.equal(findElement(render([failed, base]), "ArtifactActionCard").props.onRetry, undefined)
})

test("artifact action uses the action stream and preserves the retry request after rejection", async () => {
  const attempts = []
  const fixture = chatFixture({ thread: "t", api: {
    fetchAIChatThread: async () => ({ thread: thread("t"), messages: [] }),
    streamAIChatAction: async (id, action) => { attempts.push({ id, action }); throw new ApiError("Too many turns", 429) },
  } })
  fixture.render()
  fixture.requests[0].resolve(sources("A", ["A"]))
  await flush()
  const action = { kind: "artifact", title: "Meal", brief: "Prototype", artifactId: "existing" }
  findElement(fixture.render(), "MessageList").props.onArtifact(action)
  await flush()
  const alert = findElement(fixture.render(), "section").props.children.find((child) => child?.props?.role === "alert")
  alert.props.children[1].props.onClick()
  await flush()
  assert.deepEqual(attempts, [{ id: "t", action }, { id: "t", action }])
})

test("artifact location, expiry and page errors handle pinned, missing and invalid pages", () => {
  assert.deepEqual(artifactHelpers.artifactLocation("/artifacts/meal-swap-abcdef"), { slug: "meal-swap-abcdef", version: undefined })
  assert.deepEqual(artifactHelpers.artifactLocation("/artifacts/meal-swap-abcdef/v/2/"), { slug: "meal-swap-abcdef", version: 2 })
  assert.deepEqual(artifactHelpers.artifactLocation("/artifacts/01jabcdefghjkmnpqrstvwxyz0/v/3"), { slug: "01jabcdefghjkmnpqrstvwxyz0", version: 3 })
  for (const slug of ["01JABCDEFGHJKMNPQRSTVWXYZ0", "01jabcdefghjkmnpqrstvwxyzi", "01jabcdefghjkmnpqrstvwxyz"]) assert.equal(artifactHelpers.artifactLocation(`/artifacts/${slug}`), null)
  for (const path of ["/artifacts/../file", "/artifacts/meal-abcdef/raw", "/artifacts/meal-abcdef/v/0", "/artifacts/meal-abcdef/v/99999999999999999"]) assert.equal(artifactHelpers.artifactLocation(path), null)
  assert.equal(artifactHelpers.artifactURLExpired("/artifacts/meal-abcdef/raw?exp=100", 99000), false)
  assert.equal(artifactHelpers.artifactURLExpired("/artifacts/meal-abcdef/raw?exp=100", 100000), true)
  assert.equal(artifactHelpers.artifactPageError(404), "This artifact does not exist or was deleted.")
  assert.equal(artifactHelpers.artifactPageError(500), "Unable to load this artifact. Try again.")
})

test("artifact page loads an isolated frame and shows pinned version metadata", async () => {
  const hooks = createHooks()
  const previousLocation = window.location
  window.location = { pathname: "/artifacts/meal-abcdef/v/1", origin: "https://hub.test" }
  const artifact = { slug: "meal-abcdef", title: "Meal", ownerLogin: "ana", version: 1, currentVersion: 3, createdAt: 100000, rawUrl: "/artifacts/meal-abcdef/raw?exp=1000", downloadUrl: "/artifacts/meal-abcdef/raw?download=1", shareUrl: "https://hub.test/artifacts/meal-abcdef" }
  const { default: Page } = loadComponent("app/artifacts/page.tsx", {
    react: hooks.react, "next/link": { default: "Link" }, "lucide-react": { Download: "Download", FileCode2: "FileCode2", Loader2: "Loader2" },
    "@/lib/api": { ApiError, fetchAIChatArtifact: async (slug, version) => { assert.equal(slug, "meal-abcdef"); assert.equal(version, 1); return artifact } },
    "@/lib/auth-storage": { requestAuthToken: async () => "token" }, "@/lib/hub-url": { getHubUrl: () => "" },
    "@/lib/ai-chat-artifacts": artifactHelpers, "@/components/ui/button": { Button: "Button" }, "@/components/ai-chat/artifact-card": { CopyArtifactLink: "CopyArtifactLink", displayURL: (url) => url.replace(/^https?:\/\//, "") }, "@/hooks/use-branding": { useBranding: () => ({ appName: "Faster Factory" }) },
  })
  try {
    assert.match(elementText(hooks.render(Page)), /Loading artifact/)
    await flush()
    const tree = hooks.render(Page)
    assert.match(elementText(tree), /v1 of 3 · by ana · from AI Chat/)
    const frame = findElement(tree, "iframe")
    assert.equal(frame.props.sandbox, "allow-scripts")
    assert.equal(frame.props.referrerPolicy, "no-referrer")
    assert.equal(frame.props.src, artifact.rawUrl)
    assert.ok(findElements(tree, "Button").some((button) => elementText(button).includes("Download .html")))
  } finally { window.location = previousLocation }
})


const authRedirectHelpers = loadComponent("lib/auth-redirect.ts", {})

test("login redirects preserve shared artifact versions and reject unsafe next paths", () => {
  for (const path of ["/artifacts/meal-abcdef", "/artifacts/meal-abcdef/v/2/", "/ai-chat?thread=abc"]) {
    assert.equal(authRedirectHelpers.safeNextPath(path), path)
    const url = new URL(authRedirectHelpers.loginRedirectPath(path), "https://hub.test")
    assert.equal(url.pathname, "/login")
    assert.equal(url.searchParams.get("next"), path)
  }
  for (const path of [null, "", "https://outside.test", "//outside.test", "/\\outside.test", "/\t/outside.test", "/\n/outside.test", "relative"]) {
    assert.equal(authRedirectHelpers.safeNextPath(path), null)
    assert.equal(authRedirectHelpers.loginRedirectPath(path), "/login")
  }
})

test("artifact download refreshes an expired URL without a focus event and preserves the displayed version", async () => {
  const previousLocation = window.location
  try {
    for (const expired of [false, true]) {
      const hooks = createHooks()
      window.location = { pathname: "/artifacts/meal-abcdef", origin: "https://hub.test" }
      const expiry = Math.floor(Date.now() / 1000) + (expired ? -1 : 900)
      const artifact = { slug: "meal-abcdef", title: "Meal", ownerLogin: "ana", version: 2, currentVersion: 2, createdAt: 100000, rawUrl: `/raw?exp=${expiry}`, downloadUrl: `/raw?download=1&exp=${expiry}` }
      const fresh = { ...artifact, currentVersion: 3, rawUrl: `/raw?exp=${expiry + 900}`, downloadUrl: `/raw?download=1&exp=${expiry + 900}` }
      const requests = []
      const { default: Page } = loadComponent("app/artifacts/page.tsx", {
        react: hooks.react, "next/link": { default: "Link" }, "lucide-react": {},
        "@/lib/api": { ApiError, fetchAIChatArtifact: async (slug, version) => { requests.push({ slug, version }); return requests.length === 1 ? artifact : fresh } },
        "@/lib/auth-storage": { requestAuthToken: async () => "token" }, "@/lib/hub-url": { getHubUrl: () => "" },
        "@/lib/ai-chat-artifacts": artifactHelpers, "@/components/ui/button": { Button: "Button" }, "@/components/ai-chat/artifact-card": { CopyArtifactLink: "CopyArtifactLink", displayURL: (url) => url.replace(/^https?:\/\//, "") }, "@/hooks/use-branding": { useBranding: () => ({ appName: "Faster Factory" }) },
      })
      hooks.render(Page)
      await flush()
      const button = findElements(hooks.render(Page), "Button").find((button) => elementText(button).includes("Download .html"))
      button.props.onClick()
      await flush()
      assert.equal(window.location.href, expired ? fresh.downloadUrl : artifact.downloadUrl)
      assert.deepEqual(requests, expired ? [{ slug: artifact.slug, version: undefined }, { slug: artifact.slug, version: 2 }] : [{ slug: artifact.slug, version: undefined }])
    }
  } finally { window.location = previousLocation }
})

test("artifact API 401 returns through login to the same pinned path", async () => {
  const previousLocation = window.location
  const previousFetch = global.fetch
  window.location = { pathname: "/artifacts/meal-abcdef/v/2" }
  global.fetch = async () => ({ status: 401 })
  let cleared = false
  try {
    const { fetchAIChatArtifact } = loadComponent("lib/api.ts", {
      "./hub-url": { getHubUrl: () => "" },
      "./auth-storage": { getAuthToken: () => "expired-token", clearAuthTokens: () => { cleared = true } },
      "./auth-redirect": authRedirectHelpers,
    })
    // Session expiry deliberately leaves this promise pending until navigation.
    void fetchAIChatArtifact("meal-abcdef", 2)
    await flush()
    assert.equal(cleared, true)
    assert.equal(window.location.href, "/login?next=%2Fartifacts%2Fmeal-abcdef%2Fv%2F2")
  } finally { window.location = previousLocation; global.fetch = previousFetch }
})

const ticketTarget = { tracker: "Linear", verb: "Open Linear ticket", team: "Product", state: "Backlog", labels: ["ai-chat"] }
const ticketDraft = { ...ticketTarget, actionKey: "ticket-click-1", attemptId: "attempt-1", title: "Swap from the shopping list", body: "Clients ask 31 times a week.\n\nEvidence: 12% found swap.", summary: "Clients ask 31 times a week.", artifactId: "artifact-id" }
const ticketMocks = {
  ...artifactCardMocks,
  react: { useState: (initial) => [initial, () => {}] },
  "lucide-react": Object.fromEntries(["Check", "Copy", "ExternalLink", "FileCode2", "Loader2", "RotateCcw", "Ticket", "TriangleAlert"].map((name) => [name, name])),
  "./artifact-card": { CopyArtifactLink: "CopyArtifactLink", displayURL: (url) => url.replace(/^https?:\/\//, "") },
}

test("ticket cards show working, created and failed states, linked artifact and retry draft", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  let tree = TicketActionCard({ block: { kind: "ticket", state: "working", card: ticketDraft } })
  assert.match(elementText(tree), /Opening ticket in LinearProduct · Backlog/)
  assert.equal(tree.props.role, "status")
  const created = { ...ticketDraft, identifier: "PRO-1423", url: "https://linear.app/PRO-1423", artifact: { slug: "meal-abcdef", title: "Meal prototype", shareUrl: "https://hub.test/artifacts/meal-abcdef" } }
  tree = TicketActionCard({ block: { kind: "ticket", state: "created", card: created } })
  const text = elementText(tree)
  assert.match(text, /Linear ticketBacklogPRO-1423Swap from the shopping listClients ask 31 times a week.Team Productai-chatMeal prototype/)
  assert.match(text, /Open in Linear.*It is a plain ticket for a person to triage. Nothing else was started./)
  const links = findAll(tree, (item) => item.type === "a")
  assert.equal(links.length, 2)
  assert.equal(links[0].props.href, created.artifact.shareUrl)
  assert.equal(links[1].props.href, created.url)
  for (const link of links) { assert.equal(link.props.target, "_blank"); assert.equal(link.props.rel, "noopener noreferrer") }
  assert.equal(findElement(tree, "CopyArtifactLink").props.url, created.url)
  let retry
  tree = TicketActionCard({ block: { kind: "ticket", state: "failed", card: { ...ticketDraft, failureHeadline: "Could not create the ticket in Linear", error: "The workspace token cannot create issues in team Product." } }, onRetry: (value) => { retry = value } })
  assert.match(elementText(tree), /Could not create the ticket in LinearThe workspace token cannot create issues in team Product.Try again/)
  findElement(tree, "Button").props.onClick()
  assert.deepEqual(retry, { kind: "ticket", actionKey: ticketDraft.actionKey, title: ticketDraft.title, body: ticketDraft.body, artifactId: "artifact-id" })
  const copy = findAll(tree, (item) => typeof item.type === "function" && item.type.name === "CopyTicketText")[0]
  assert.equal(copy.props.title, ticketDraft.title)
  assert.equal(copy.props.body, ticketDraft.body)
  tree = TicketActionCard({ block: { kind: "ticket", state: "failed", card: ticketDraft } })
  assert.equal(findElement(tree, "Button"), undefined)
  // Display names come from the server, including for a future tracker sibling.
  tree = TicketActionCard({ block: { kind: "ticket", state: "created", card: { ...created, tracker: "Example" } } })
  assert.match(elementText(tree), /Example ticket/)
  assert.match(elementText(tree), /Open in Example/)
  assert.doesNotMatch(elementText(tree), /Linear/)
})

test("ticket cards drop the link of a deleted artifact and retry without it", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const created = { ...ticketDraft, identifier: "PRO-1423", url: "https://linear.app/PRO-1423", artifact: { slug: "meal-abcdef", title: "Meal prototype", shareUrl: "https://hub.test/artifacts/meal-abcdef" }, artifactDeleted: true }
  let tree = TicketActionCard({ block: { kind: "ticket", state: "created", card: created } })
  assert.match(elementText(tree), /Meal prototypeThis artifact was deleted\./)
  assert.deepEqual(findAll(tree, (item) => item.type === "a").map((link) => link.props.href), [created.url])
  let retry
  tree = TicketActionCard({ block: { kind: "ticket", state: "failed", card: { ...ticketDraft, artifactDeleted: true, error: "Could not create the ticket." } }, onRetry: (value) => { retry = value } })
  findElement(tree, "Button").props.onClick()
  assert.equal(retry.artifactId, undefined)
})

function findAll(tree, predicate) {
  if (!tree || typeof tree !== "object") return []
  return [...(predicate(tree) ? [tree] : []), ...(Array.isArray(tree) ? tree : [tree.props?.children]).flatMap((child) => findAll(child, predicate))]
}

test("copy ticket as text preserves the draft and shows success or failure feedback", async () => {
  for (const failure of [false, true]) {
    const hooks = createHooks()
    const { CopyTicketText } = loadComponent("components/ai-chat/ticket-card.tsx", { ...ticketMocks, react: hooks.react })
    const previous = Object.getOwnPropertyDescriptor(global, "navigator")
    let copied
    Object.defineProperty(global, "navigator", { configurable: true, value: { clipboard: { writeText: async (text) => { if (failure) throw new Error("denied"); copied = text } } } })
    try {
      let tree = hooks.render(() => CopyTicketText(ticketDraft))
      assert.match(elementText(tree), /Copy as text/)
      tree.props.onClick()
      await flush()
      tree = hooks.render(() => CopyTicketText(ticketDraft))
      assert.match(elementText(tree), failure ? /Could not copy/ : /Copied/)
      if (!failure) assert.equal(copied, `${ticketDraft.title}\n\n${ticketDraft.body}`)
    } finally { if (previous) Object.defineProperty(global, "navigator", previous); else delete global.navigator }
  }
})

test("ticket wrap-up uses server verb and configured target subtitle", () => {
  const actions = []
  const block = { id: "wrap", type: "actions", actions: ["artifact", "ticket"], ticket: ticketTarget }
  const tree = blockHelpers.InlineBlock({ block, onAction: (value) => actions.push(value) })
  assert.match(elementText(tree), /Open Linear ticketProduct · Backlog/)
  findAll(tree, (item) => item.type === "Button")[1].props.onClick()
  assert.deepEqual(actions, ["ticket"])
  const withoutState = blockHelpers.InlineBlock({ block: { ...block, ticket: { ...ticketTarget, state: "", verb: "Create Example issue" } }, onAction() {} })
  assert.match(elementText(withoutState), /Create Example issueProduct/)
  assert.doesNotMatch(elementText(withoutState), /Backlog|Product ·/)
})

test("created artifact offers a ticket only with availability and a latest-message handler", () => {
  const { ArtifactActionCard } = loadComponent("components/ai-chat/artifact-card.tsx", ticketMocks)
  const block = { kind: "artifact", state: "created", card: { title: "Meal", slug: "meal-abcdef", artifactId: "a", version: 1 } }
  let action
  const onTicket = (value) => { action = value }
  let tree = ArtifactActionCard({ block, ticket: ticketTarget, onTicket })
  assert.match(elementText(tree), /Open Linear ticket with this artifact/)
  findAll(tree, (item) => item.type === "Button" && elementText(item).includes("with this artifact"))[0].props.onClick()
  assert.deepEqual(action, { kind: "ticket", artifactId: "a" })
  for (const props of [{ block, onTicket }, { block, ticket: ticketTarget }, { block: { ...block, state: "working" }, ticket: ticketTarget, onTicket }, { block: { ...block, state: "failed" }, ticket: ticketTarget, onTicket }]) {
    tree = ArtifactActionCard(props)
    assert.doesNotMatch(elementText(tree), /with this artifact/)
  }
})

test("ticket SSE replaces a single card without overwriting an artifact card", () => {
  const { applyStreamEvent } = loadComponent("components/ai-chat/stream-turn.ts", {})
  let messages = []
  const turn = { threadId: "t", text: "", retry: false, action: { kind: "ticket" }, assistantId: "" }
  const view = { setMessages: (fn) => { messages = fn(messages) }, setDraft() { throw new Error("cleared draft") }, setReading() {} }
  applyStreamEvent(turn, "message_started", { messageId: "m" }, view)
  applyStreamEvent(turn, "action", { kind: "artifact", state: "created", card: { title: "Meal", slug: "meal-abcdef" } }, view)
  for (const state of ["working", "working", "failed"]) applyStreamEvent(turn, "action", { kind: "ticket", state, card: { ...ticketDraft, error: state === "failed" ? "Failed" : undefined } }, view)
  assert.equal(messages.length, 1)
  assert.equal(messages[0].blocks.length, 2)
  assert.equal(messages[0].blocks[0].kind, "artifact")
  assert.equal(messages[0].blocks[1].id, "m-ticket")
  assert.equal(messages[0].blocks[1].state, "failed")
  assert.equal(messages[0].blocks[1].card.body, ticketDraft.body)
})

test("ticket retry and artifact ticket links are restricted to the latest available message", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock" }, "./artifact-card": { ArtifactActionCard: "ArtifactActionCard" }, "./ticket-card": { TicketActionCard: "TicketActionCard" },
  })
  const base = { id: "m", role: "assistant", content: "", createdAt: 0, status: "completed" }
  const ticket = { ...base, blocks: [{ id: "t", type: "action", kind: "ticket", state: "failed", at: 0, card: ticketDraft }] }
  const artifact = { ...base, blocks: [{ id: "a", type: "action", kind: "artifact", state: "created", at: 0, card: { title: "Meal", artifactId: "a" } }] }
  const onTicket = () => {}
  const render = (messages, props = {}) => hooks.render(() => MessageList({ messages, reading: "", onTicket, ticket: ticketTarget, ...props }))
  assert.equal(findElement(render([ticket]), "TicketActionCard").props.onRetry, onTicket)
  assert.equal(findElement(render([artifact]), "ArtifactActionCard").props.onTicket, onTicket)
  for (const props of [{ streaming: true }, { interviewDisabled: true }, { ticket: undefined }]) {
    assert.equal(findElement(render([ticket], props), "TicketActionCard").props.onRetry, undefined)
    assert.equal(findElement(render([artifact], props), "ArtifactActionCard").props.onTicket, undefined)
  }
  assert.equal(findElement(render([ticket, { ...base, id: "new" }]), "TicketActionCard").props.onRetry, undefined)
  assert.equal(findElement(render([artifact, { ...base, id: "new" }]), "ArtifactActionCard").props.onTicket, undefined)
})

test("ticket action reaches the action stream with the retry draft unchanged", async () => {
  const attempts = []
  const fixture = chatFixture({ thread: "t", api: {
    fetchAIChatThread: async () => ({ thread: thread("t"), messages: [] }),
    streamAIChatAction: async (id, action) => { attempts.push({ id, action }); throw new ApiError("Busy", 429) },
  } })
  fixture.render()
  fixture.requests[0].resolve({ ...sources("A", ["A"]), actions: ["artifact", "ticket"], ticket: ticketTarget })
  await flush()
  const action = { kind: "ticket", title: ticketDraft.title, body: ticketDraft.body, artifactId: "a" }
  const list = findElement(fixture.render(), "MessageList")
  assert.deepEqual(list.props.ticket, ticketTarget)
  list.props.onTicket(action)
  await flush()
  const alert = findElement(fixture.render(), "section").props.children.find((child) => child?.props?.role === "alert")
  alert.props.children[1].props.onClick()
  await flush()
  assert.match(attempts[0].action.actionKey, /^[a-f0-9-]{36}$/)
  assert.deepEqual(attempts, [{ id: "t", action: { ...action, actionKey: attempts[0].action.actionKey } }, { id: "t", action: attempts[0].action }])
})

test("tracker source badge reflects create-ticket access from the API", () => {
  const { ChatHeader } = loadComponent("components/ai-chat/header.tsx", {
    "lucide-react": {}, "@/components/ui/button": { Button: "Button" }, "@/components/ui/popover": { Popover: "Popover", PopoverContent: "PopoverContent", PopoverTrigger: "PopoverTrigger" },
    "./mode-cards": { chatModes: [] }, "./sources": loadComponent("components/ai-chat/sources.ts", {}),
  })
  for (const available of [false, true]) {
    const tree = ChatHeader({ mode: null, data: { workspace: "A", ticket: available ? ticketTarget : undefined, sources: [{ kind: "issue_tracker", name: "Linear", status: "connected", access: available ? "read_create_ticket" : "read" }] } })
    if (available) assert.match(elementText(tree), /Read \+ create ticket/)
    else assert.doesNotMatch(elementText(tree), /Read \+ create ticket/)
  }
})

test("unknown ticket outcomes do not claim rejection, and cancellation does not claim a write", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const unknown = TicketActionCard({ block: { kind: "ticket", state: "failed", card: { ...ticketDraft, outcomeUnknown: true, error: "Linear did not confirm the ticket. Check Linear before trying again." } } })
  assert.match(elementText(unknown), /Linear did not confirm the ticket/)
  assert.doesNotMatch(elementText(unknown), /did not accept/)
  const stopped = TicketActionCard({ block: { kind: "ticket", state: "failed", card: { ...ticketDraft, error: "Stopped. Nothing was created." } } })
  assert.match(elementText(stopped), /Ticket creation stopped/)
})

test("created ticket follows its completion sentence during streaming as well as reload", () => {
  const { applyStreamEvent } = loadComponent("components/ai-chat/stream-turn.ts", {})
  let messages = []
  const turn = { threadId: "t", action: { kind: "ticket" }, text: "", assistantId: "" }
  const view = { setMessages: (fn) => { messages = fn(messages) }, setDraft() {}, setReading() {} }
  applyStreamEvent(turn, "message_started", { messageId: "m" }, view)
  applyStreamEvent(turn, "action", { kind: "ticket", state: "working", card: ticketDraft }, view)
  const text = "Done. The ticket is in the Product backlog."
  applyStreamEvent(turn, "token", { text }, view)
  applyStreamEvent(turn, "action", { kind: "ticket", state: "created", card: { ...ticketDraft, identifier: "PRO-1423", url: "https://linear.app/PRO-1423" } }, view)
  const [sentence, block] = blockHelpers.splitContent(messages[0].content, messages[0].blocks)
  assert.equal(sentence, text)
  assert.equal(block.kind, "ticket")
  assert.equal(block.at, Array.from(text).length)
})

test("a saved wrap-up stops offering tickets when the workspace loses access", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {}, "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock" },
  })
  const message = { id: "m", role: "assistant", content: "", createdAt: 0, status: "completed", blocks: [{ id: "wrap", at: 0, type: "actions", actions: ["ticket"], ticket: ticketTarget }] }
  const tree = hooks.render(() => MessageList({ messages: [message], reading: "", onArtifact() {}, onTicket() {} }))
  const props = findElement(tree, "InlineBlock").props
  assert.deepEqual(props.block.actions, [])
  assert.equal(blockHelpers.InlineBlock(props), null)
})


test("pre-provider ticket failures use the server headline and hide empty copy text", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  for (const error of ["Could not draft the ticket.", "This artifact does not exist in this conversation.", "Ticket creation is not available.", "Unable to save the ticket draft."]) {
    const card = { ...ticketDraft, title: "", body: "", failureHeadline: "Could not prepare the ticket", error }
    const tree = TicketActionCard({ block: { kind: "ticket", state: "failed", card } })
    assert.match(elementText(tree), /Could not prepare the ticket/)
    assert.doesNotMatch(elementText(tree), /did not accept/)
    assert.equal(findAll(tree, (item) => typeof item.type === "function" && item.type.name === "CopyTicketText").length, 0)
  }
  // Older persisted cards have a neutral fallback, even without a failure phase.
  const legacy = TicketActionCard({ block: { kind: "ticket", state: "failed", card: { ...ticketDraft, error: "Could not draft the ticket." } } })
  assert.match(elementText(legacy), /Could not create the ticket/)
  assert.doesNotMatch(elementText(legacy), /did not accept/)
})

test("unknown ticket outcome requires an explicitly labeled acknowledgement and preserves the draft", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  let retry
  const tree = TicketActionCard({
    block: { kind: "ticket", state: "failed", card: { ...ticketDraft, outcomeUnknown: true, error: "Check Linear before trying again." } },
    onRetry: (action) => { retry = action },
  })
  assert.equal(retry, undefined)
  assert.match(elementText(tree), /Check Linear before trying again/)
  assert.doesNotMatch(elementText(tree), /Try again/)
  const button = findElement(tree, "Button")
  assert.equal(elementText(button), "I checked Linear — create anyway")
  button.props.onClick()
  assert.deepEqual(retry, { kind: "ticket", title: ticketDraft.title, body: ticketDraft.body, artifactId: ticketDraft.artifactId, actionKey: ticketDraft.actionKey, confirmUnknown: ticketDraft.attemptId })
  const original = retry
  const second = TicketActionCard({
    block: { kind: "ticket", state: "failed", card: { ...ticketDraft, attemptId: "attempt-2", confirmedUnknown: ticketDraft.attemptId, outcomeUnknown: true } },
    onRetry: (action) => { retry = action },
  })
  findElement(second, "Button").props.onClick()
  assert.equal(retry.confirmUnknown, "attempt-2")
  assert.equal(original.confirmUnknown, ticketDraft.attemptId)
})

test("creating anyway resolves an unknown card without claiming the user found the ticket", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": blockHelpers, "./ticket-card": { TicketActionCard: "TicketActionCard" },
  })
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const error = "Linear did not confirm the ticket. Check Linear before trying again."
  const unknown = { id: "a", role: "assistant", content: "", createdAt: 0, status: "completed", blocks: [{ id: "a-ticket", type: "action", kind: "ticket", state: "failed", at: 0, card: { ...ticketDraft, attemptId: "a", outcomeUnknown: true, error } }] }
  const created = { ...unknown, id: "b", blocks: [{ ...unknown.blocks[0], id: "b-ticket", state: "created", card: { ...ticketDraft, attemptId: "b", confirmedUnknown: "a", identifier: "PRO-7", url: "https://linear.app/PRO-7" } }] }
  const tree = hooks.render(() => MessageList({ messages: [unknown, created], reading: "", ticket: ticketTarget, onTicket() {}, onDismissTicket() {} }))
  const cards = findElements(tree, "TicketActionCard")
  assert.equal(cards[0].props.onRetry, undefined)
  assert.equal(cards[0].props.onDismiss, undefined)
  assert.ok(!cards[0].props.block.card.dismissed)
  const originalText = elementText(TicketActionCard(cards[0].props))
  assert.ok(originalText.includes(error))
  assert.doesNotMatch(originalText, /You confirmed the ticket exists|create anyway|It's already in/)
  assert.match(elementText(TicketActionCard(cards[1].props)), /PRO-7/)
})

test("unknown confirmation remains reachable after chatting and created replays", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock" }, "./ticket-card": { TicketActionCard: "TicketActionCard" },
  })
  const base = { role: "assistant", content: "", createdAt: 0, status: "completed" }
  const unknown = { ...base, id: "unknown", blocks: [{ id: "ticket", type: "action", kind: "ticket", state: "failed", at: 0, card: { ...ticketDraft, outcomeUnknown: true } }] }
  const chat = { ...base, id: "chat", content: "Continuing the discussion" }
  const onTicket = () => {}
  const render = (messages, props = {}) => hooks.render(() => MessageList({ messages, reading: "", ticket: ticketTarget, onTicket, ...props }))
  assert.equal(findElement(render([unknown, chat]), "TicketActionCard").props.onRetry, onTicket)
  assert.equal(findElement(render([unknown, chat], { streaming: true }), "TicketActionCard").props.disabled, true)
  const newer = { ...unknown, id: "newer", blocks: [{ ...unknown.blocks[0], id: "newer-ticket", state: "created", card: ticketDraft }] }
  assert.equal(findElement(render([unknown, chat, newer]), "TicketActionCard").props.onRetry, onTicket)
  const confirmed = { ...newer, blocks: [{ ...newer.blocks[0], card: { ...ticketDraft, attemptId: "attempt-2", confirmedUnknown: ticketDraft.attemptId } }] }
  assert.equal(findElement(render([unknown, chat, confirmed]), "TicketActionCard").props.onRetry, undefined)
  const secondUnknown = { ...confirmed, blocks: [{ ...confirmed.blocks[0], state: "failed", card: { ...confirmed.blocks[0].card, outcomeUnknown: true } }] }
  const cards = findElements(render([unknown, secondUnknown, newer]), "TicketActionCard")
  assert.equal(cards[0].props.onRetry, undefined)
  assert.equal(cards[1].props.onRetry, onTicket)
})

for (const status of [409, "lost stream"]) test(`ticket retry preserves its action key and displays the server error (${status})`, async () => {
  const attempts = []
  let loads = 0
  const message = "Example did not confirm the previous ticket. Check Example first, then use the ticket card to confirm creating another ticket."
  const saved = { id: "unknown", role: "assistant", status: "completed", content: "", createdAt: 0, blocks: [{ id: "ticket", type: "action", kind: "ticket", state: "failed", at: 0, card: { ...ticketDraft, outcomeUnknown: true } }] }
  const fixture = chatFixture({ thread: "t", api: {
    fetchAIChatThread: async () => {
      if (loads++ && status === "lost stream") throw new Error("offline")
      return { thread: thread("t"), messages: [saved, { id: "chat", role: "assistant", content: "Still chatting", status: "completed", createdAt: 0 }] }
    },
    streamAIChatAction: async (_id, action) => {
      attempts.push(action)
      if (status === 409) throw new ApiError(message, 409)
      throw new Error("connection lost")
    },
  } })
  fixture.render()
  fixture.requests[0].resolve({ ...sources("A", ["A"]), actions: ["artifact", "ticket"], ticket: ticketTarget })
  await flush()
  findElement(fixture.render(), "MessageList").props.onTicket({ kind: "ticket", confirmUnknown: ticketDraft.attemptId })
  await flush()
  let tree = fixture.render()
  assert.match(elementText(tree), status === 409 ? /Example did not confirm the previous ticket/ : /connection lost/)
  const alert = findElement(tree, "section").props.children.find((child) => child?.props?.role === "alert")
  alert.props.children[1].props.onClick()
  await flush()
  assert.match(attempts[0].actionKey, /^[a-f0-9-]{36}$/)
  assert.deepEqual(attempts[1], attempts[0])
  assert.equal(attempts[1].confirmUnknown, ticketDraft.attemptId)
  // A separate user click gets a new key.
  findElement(fixture.render(), "MessageList").props.onTicket()
  await flush()
  assert.notEqual(attempts[2].actionKey, attempts[0].actionKey)
})

test("unknown ticket dismissal is available beside confirmation and displays a durable acknowledgement", () => {
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const block = { id: "attempt-1-ticket", kind: "ticket", state: "failed", card: { ...ticketDraft, tracker: "Example", outcomeUnknown: true, error: "Check Example first." } }
  const props = { block, onRetry() {}, onDismiss: async () => {} }
  let tree = TicketActionCard(props)
  const buttons = findElements(tree, "Button")
  assert.deepEqual(buttons.map(elementText), ["I checked Example — create anyway", "It's already in Example"])
  assert.equal(buttons[1].props.variant, "outline")
  for (const overrides of [{ onRetry: undefined }, { block: { ...block, card: ticketDraft } }, { block: { ...block, state: "created" } }, { block: { ...block, state: "working" } }]) {
    assert.doesNotMatch(elementText(TicketActionCard({ ...props, ...overrides })), /It's already in/)
  }
  tree = TicketActionCard({ ...props, block: { ...block, card: { ...block.card, dismissed: true } } })
  assert.match(elementText(tree), /Example did not confirm the ticketYou confirmed the ticket exists in Example\./)
  assert.doesNotMatch(elementText(tree), /Check Example first|I checked|It's already in|Try again/)
  assert.equal(findAll(tree, (item) => typeof item.type === "function" && item.type.name === "CopyTicketText").length, 1)
})

test("ticket dismissal disables both buttons while pending or streaming and reports an inline error", async () => {
  const hooks = createHooks()
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", { ...ticketMocks, react: hooks.react })
  let rejectDismiss, dismissed
  const props = {
    block: { id: "attempt-1-ticket", kind: "ticket", state: "failed", card: { ...ticketDraft, outcomeUnknown: true } },
    onRetry() {},
    onDismiss: (id) => { dismissed = id; return new Promise((_resolve, reject) => { rejectDismiss = reject }) },
  }
  const render = (disabled = false) => hooks.render(() => TicketActionCard({ ...props, disabled }))
  assert.ok(findElements(render(true), "Button").every((button) => button.props.disabled))
  findElements(render(), "Button")[1].props.onClick()
  assert.equal(dismissed, ticketDraft.attemptId)
  assert.ok(findElements(render(), "Button").every((button) => button.props.disabled))
  rejectDismiss(new Error("Unable to dismiss ticket attempt"))
  await flush()
  const tree = render()
  assert.ok(findElements(tree, "Button").every((button) => !button.props.disabled))
  assert.match(elementText(tree), /Unable to dismiss ticket attempt/)
  assert.equal(findElements(tree, "p")[0].props.role, "alert")
})

test("only the latest unresolved unknown card offers dismissal, including after a repeated refusal", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": { ...blockHelpers, InlineBlock: "InlineBlock" }, "./ticket-card": { TicketActionCard: "TicketActionCard" },
  })
  const unknown = { id: "m", role: "assistant", content: "", createdAt: 0, status: "completed", blocks: [{ id: "m-ticket", type: "action", kind: "ticket", state: "failed", at: 0, card: { ...ticketDraft, outcomeUnknown: true } }] }
  const repeated = { ...unknown, id: "repeat", blocks: [{ ...unknown.blocks[0], id: "repeat-ticket" }] }
  const onDismissTicket = async () => {}
  const render = (messages) => hooks.render(() => MessageList({ messages, reading: "", ticket: ticketTarget, onTicket() {}, onDismissTicket }))
  let cards = findElements(render([unknown, repeated]), "TicketActionCard")
  assert.equal(cards[0].props.onDismiss, undefined)
  assert.equal(cards[1].props.onDismiss, onDismissTicket)
  const dismissed = { ...unknown, blocks: [{ ...unknown.blocks[0], card: { ...unknown.blocks[0].card, dismissed: true } }] }
  cards = findElements(render([dismissed, repeated]), "TicketActionCard")
  assert.ok(cards.every((card) => !card.props.onRetry && !card.props.onDismiss))
  assert.equal(cards[1].props.block.card.dismissed, true)
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const repeatedText = elementText(TicketActionCard(cards[1].props))
  assert.match(repeatedText, /You confirmed the ticket exists in Linear/)
  assert.doesNotMatch(repeatedText, /Check Linear before trying again/)
})

for (const navigate of [false, true]) test(`ticket dismissal resyncs only the conversation it belongs to (navigate=${navigate})`, async () => {
  let finishDismiss
  const loads = [], dismissals = []
  const saved = { id: "unknown", role: "assistant", status: "completed", content: "", createdAt: 0, blocks: [{ id: "ticket", type: "action", kind: "ticket", state: "failed", at: 0, card: { ...ticketDraft, outcomeUnknown: true } }] }
  const fixture = chatFixture({ thread: "t", api: {
    fetchAIChatThread: async (id) => { loads.push(id); return { thread: thread(id), messages: [{ ...saved, blocks: [{ ...saved.blocks[0], card: { ...saved.blocks[0].card, dismissed: dismissals.length > 0 } }] }] } },
    dismissAIChatTicketAttempt: (id, attemptId) => { dismissals.push({ id, attemptId }); return new Promise((resolve) => { finishDismiss = resolve }) },
  } })
  fixture.render()
  fixture.requests[0].resolve({ ...sources("A", ["A"]), actions: ["ticket"], ticket: ticketTarget })
  await flush()
  const pending = findElement(fixture.render(), "MessageList").props.onDismissTicket(ticketDraft.attemptId)
  assert.deepEqual(dismissals, [{ id: "t", attemptId: ticketDraft.attemptId }])
  if (navigate) {
    fixture.navigate("other")
    fixture.render()
    await flush()
  }
  finishDismiss()
  await pending
  const list = findElement(fixture.render(), "MessageList")
  assert.deepEqual(loads, navigate ? ["t", "other"] : ["t", "t"])
  assert.equal(list.props.messages[0].blocks[0].card.dismissed, true)
})

test("ticket dismissal uses an authenticated non-streaming POST and accepts 204", async () => {
  const { dismissAIChatTicketAttempt } = loadComponent("lib/api.ts", {
    "./hub-url": { getHubUrl: () => "https://hub.test" },
    "./auth-storage": { getAuthToken: () => "session-token" },
  })
  const original = global.fetch
  let request
  global.fetch = async (url, options) => { request = { url, options }; return { ok: true, status: 204 } }
  try {
    await dismissAIChatTicketAttempt("thread/id", "attempt/id")
    assert.equal(request.url, "https://hub.test/api/ai-chat/threads/thread%2Fid/ticket-attempts/attempt%2Fid/dismiss")
    assert.equal(request.options.method, "POST")
    assert.equal(request.options.headers.Authorization, "Bearer session-token")
    assert.equal(request.options.body, undefined)
    assert.equal(request.options.headers.Accept, undefined)
  } finally { global.fetch = original }
})

test("ticket action keys fall back outside secure contexts and key failures clear streaming", async () => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "crypto")
  try {
    for (const fail of [false, true]) {
      Object.defineProperty(globalThis, "crypto", { configurable: true, value: { randomUUID: undefined, getRandomValues: (bytes) => {
        if (fail) throw new Error("Random source unavailable")
        return bytes.fill(171)
      } } })
      const attempts = []
      const fixture = chatFixture({ thread: "t", api: {
        fetchAIChatThread: async () => ({ thread: thread("t"), messages: [] }),
        streamAIChatAction: async (_id, action) => { attempts.push(action) },
      } })
      fixture.render()
      fixture.requests[0].resolve({ ...sources("A", ["A"]), actions: ["ticket"], ticket: ticketTarget })
      await flush()
      await findElement(fixture.render(), "MessageList").props.onTicket()
      const tree = fixture.render()
      assert.equal(findElement(tree, "ChatComposer").props.streaming, false)
      if (fail) {
        assert.equal(attempts.length, 0)
        assert.match(elementText(tree), /Random source unavailable/)
        // A subsequent successful click can still start a turn.
        globalThis.crypto.getRandomValues = (bytes) => bytes.fill(1)
        await findElement(tree, "MessageList").props.onTicket()
        assert.equal(attempts.length, 1)
      } else {
        assert.equal(attempts.length, 1)
        assert.equal(attempts[0].actionKey, "ab".repeat(16))
        assert.match(attempts[0].actionKey, /^[A-Za-z0-9-]{1,64}$/)
      }
    }
  } finally { Object.defineProperty(globalThis, "crypto", descriptor) }
})

test("a persisted streaming ticket shows its working card without Thinking", () => {
  const hooks = createHooks()
  const { MessageList } = loadComponent("components/ai-chat/message-list.tsx", {
    react: hooks.react, "react-markdown": {}, "remark-gfm": {}, "lucide-react": {},
    "./blocks": blockHelpers, "./ticket-card": { TicketActionCard: "TicketActionCard" },
  })
  const { TicketActionCard } = loadComponent("components/ai-chat/ticket-card.tsx", ticketMocks)
  const message = { id: "working", role: "assistant", content: "", status: "streaming", createdAt: 0, blocks: [{ id: "working-ticket", type: "action", kind: "ticket", state: "working", at: 0, card: ticketDraft }] }
  const tree = hooks.render(() => MessageList({ messages: [message], reading: "", streaming: false }))
  assert.doesNotMatch(elementText(tree), /Thinking/)
  assert.match(elementText(TicketActionCard(findElement(tree, "TicketActionCard").props)), /Opening ticket in Linear/)
})

for (const outcome of ["completed", "bounded", "navigate"]) test(`Stop resyncs a detached ticket until ${outcome}`, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] })
  let loads = 0
  const working = { id: "working", role: "assistant", content: "", status: "streaming", createdAt: 0, blocks: [{ id: "working-ticket", type: "action", kind: "ticket", state: "working", at: 0, card: ticketDraft }] }
  const fixture = chatFixture({ thread: "t", api: {
    fetchAIChatThread: async (id) => {
      loads++
      const complete = outcome === "completed" && loads >= 4
      return { thread: thread(id), messages: loads === 1 || id !== "t" ? [] : [{ ...working, status: complete ? "cancelled" : "streaming", blocks: [{ ...working.blocks[0], state: complete ? "created" : "working" }] }] }
    },
    streamAIChatAction: (_id, _action, signal, onEvent) => {
      onEvent("message_started", { messageId: "working" })
      onEvent("action", { kind: "ticket", state: "working", card: ticketDraft })
      return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new Error("aborted"))))
    },
  } })
  fixture.render()
  fixture.requests[0].resolve({ ...sources("A", ["A"]), actions: ["ticket"], ticket: ticketTarget })
  await flush()
  const pending = findElement(fixture.render(), "MessageList").props.onTicket()
  await flush()
  findElement(fixture.render(), "ChatComposer").props.onCancel()
  await pending
  let tree = fixture.render()
  assert.equal(findElement(tree, "ChatComposer").props.streaming, false)
  assert.equal(findElement(tree, "MessageList").props.messages[0].blocks[0].state, "working")
  if (outcome === "navigate") {
    fixture.navigate("other")
    fixture.render()
    await flush()
    fixture.render()
  }
  for (let i = 0; i < 25; i++) {
    t.mock.timers.tick(2000)
    await flush()
    tree = fixture.render()
  }
  assert.equal(loads, outcome === "completed" ? 4 : outcome === "bounded" ? 22 : 3)
  if (outcome === "completed") assert.equal(findElement(tree, "MessageList").props.messages[0].blocks[0].state, "created")
  if (outcome === "navigate") assert.deepEqual(findElement(tree, "MessageList").props.messages, [])
})

function historyFixture(api = {}) {
  const hooks = createHooks()
  const navigation = []
  const { HistoryPopover, historyTime } = loadComponent("components/ai-chat/history-popover.tsx", {
    react: hooks.react,
    "next/navigation": { useRouter: () => ({ replace: (url) => navigation.push(url) }) },
    "lucide-react": { Eye: "Eye", FileCode2: "FileCode2", History: "History", Search: "Search", Ticket: "Ticket", Trash2: "Trash2" },
    "@/components/ui/button": { Button: "Button" },
    "@/components/ui/input": { Input: "Input" },
    "@/components/ui/popover": { Popover: "Popover", PopoverContent: "PopoverContent", PopoverTrigger: "PopoverTrigger" },
    "@/components/ui/tabs": { Tabs: "Tabs", TabsContent: "TabsContent", TabsList: "TabsList", TabsTrigger: "TabsTrigger" },
    "./mode-cards": { chatModes: [{ id: "explore_idea", name: "Explore idea", icon: "Lightbulb" }] },
    // The web UI may run on another origin than the hub that serves artifact pages.
    "@/lib/ai-chat-artifacts": loadComponent("lib/ai-chat-artifacts.ts", { "./hub-url": { getHubUrl: () => "https://hub.test" } }),
    "@/lib/api": { fetchAIChatThreads: async () => [], fetchAIChatArtifacts: async () => [], ...api },
  })
  const render = () => hooks.render(() => HistoryPopover({ threadId: "today" }))
  return { render, historyTime, navigation, open: () => findElement(render(), "Popover").props.onOpenChange(true) }
}

test("history groups rows by calendar time, highlights current chat and shows badges", async () => {
  const now = new Date(2026, 9, 15, 12).getTime()
  const rows = [
    { id: "today", title: "Prototype", mode: "explore_idea", updatedAt: now, artifactCount: 2, ticket: "PRO-1423" },
    { id: "week", title: "Recent", mode: "explore_idea", updatedAt: new Date(2026, 9, 12).getTime() },
    { id: "old", title: "Earlier", mode: "explore_idea", updatedAt: new Date(2025, 8, 1).getTime() },
  ]
  const fixture = historyFixture({ fetchAIChatThreads: async () => rows })
  assert.deepEqual(fixture.historyTime(now, now), { group: "Today", label: "12:00" })
  assert.equal(fixture.historyTime(rows[1].updatedAt, now).group, "Last 7 days")
  assert.match(fixture.historyTime(rows[2].updatedAt, now).label, /2025/)
  // A row from the same weekday a week ago would repeat today's weekday label.
  assert.equal(fixture.historyTime(new Date(2026, 9, 9, 12).getTime(), now).group, "Last 7 days")
  assert.equal(fixture.historyTime(new Date(2026, 9, 8, 12).getTime(), now).group, "Older")
  const clock = Date.now
  Date.now = () => now
  let tree
  try {
    fixture.open()
    assert.match(elementText(fixture.render()), /Loading history/)
    await flush()
    tree = fixture.render()
  } finally { Date.now = clock }
  assert.match(elementText(tree), /Today.*Prototype.*Explore idea.*12:00.*2.*PRO-1423.*Last 7 days.*Recent.*Older.*Earlier/s)
  assert.match(elementText(tree), /Only you can see your chats/)
  const current = findElements(tree, "button").find((row) => row.props["aria-current"] === "page")
  assert.ok(current.props.className.includes("bg-accent"))
  current.props.onClick()
  assert.deepEqual(fixture.navigation, ["/chat?thread=today"])
  assert.equal(findElement(fixture.render(), "Popover").props.open, false)
})

test("history handles empty, error and retry states inline", async () => {
  let fail = true
  const fixture = historyFixture({ fetchAIChatThreads: async () => { if (fail) throw Error("offline"); return [] } })
  fixture.open()
  await flush()
  assert.match(elementText(fixture.render()), /Unable to load history/)
  fail = false
  findElements(fixture.render(), "Button").find((button) => elementText(button) === "Retry").props.onClick()
  await flush()
  const tree = fixture.render()
  assert.match(elementText(tree), /No chats yet/)
  assert.match(elementText(tree), /No artifacts yet. Ask for a prototype in any chat/)
})

test("history artifact deletion confirms, removes the row and opens its chat", async () => {
  const deleted = []
  const previous = window.dispatchEvent
  window.dispatchEvent = () => {}
  try {
    const artifact = { id: "a", slug: "prototype-abcdef", title: "Prototype", currentVersion: 3, threadId: "t", threadTitle: "Chat", updatedAt: Date.now(), url: "/artifacts/prototype-abcdef" }
    const fixture = historyFixture({ fetchAIChatArtifacts: async () => [artifact], deleteAIChatArtifact: async (slug) => { deleted.push(slug) } })
    fixture.open()
    await flush()
    findElement(fixture.render(), "Tabs").props.onValueChange("artifacts")
    let tree = fixture.render()
    assert.match(elementText(tree), /v3/)
    // The list is private; artifact links open for anyone signed in to the hub.
    assert.match(elementText(tree), /Only you see this list\. Anyone signed in to the hub can open an artifact link\./)
    const link = findElement(tree, "a")
    assert.equal(link.props.href, "https://hub.test/artifacts/prototype-abcdef")
    assert.equal(link.props.target, "_blank")
    findElements(tree, "button").find((b) => elementText(b) === "Open chat").props.onClick()
    assert.deepEqual(fixture.navigation, ["/chat?thread=t"])
    fixture.open()
    await flush()
    const trash = () => findElements(fixture.render(), "Button").find((b) => b.props["aria-label"] === "Delete Prototype")
    trash().props.onClick()
    assert.match(elementText(fixture.render()), /Delete this artifact\? The link stops working\./)
    findElements(fixture.render(), "Button").find((b) => elementText(b) === "Cancel").props.onClick()
    assert.doesNotMatch(elementText(fixture.render()), /Delete this artifact/)
    assert.deepEqual(deleted, [])
    trash().props.onClick()
    findElements(fixture.render(), "Button").find((b) => elementText(b) === "Delete").props.onClick()
    await flush()
    assert.deepEqual(deleted, [artifact.slug])
    assert.equal(findElement(fixture.render(), "a"), undefined)
    assert.match(elementText(fixture.render()), /No artifacts yet/)
  } finally { window.dispatchEvent = previous }
})

test("history search filters rows by title in each tab", async () => {
  const now = Date.now()
  const fixture = historyFixture({
    fetchAIChatThreads: async () => [{ id: "a", title: "Swap meals", mode: "explore_idea", updatedAt: now }, { id: "b", title: "Quiz completion", mode: "explore_idea", updatedAt: now }],
    fetchAIChatArtifacts: async () => [{ id: "x", slug: "meal-swap", title: "Meal swap prototype", currentVersion: 1, threadId: "a", threadTitle: "Swap meals", updatedAt: now, url: "/artifacts/meal-swap" }],
  })
  fixture.open()
  await flush()
  const search = () => findElement(fixture.render(), "Input")
  assert.equal(search().props.placeholder, "Search my chats")
  search().props.onChange({ target: { value: "QUIZ" } })
  let text = elementText(fixture.render())
  assert.match(text, /Quiz completion/)
  assert.doesNotMatch(text, /Swap meals/)
  search().props.onChange({ target: { value: "nothing" } })
  assert.match(elementText(fixture.render()), /No chats match\./)
  findElement(fixture.render(), "Tabs").props.onValueChange("artifacts")
  assert.equal(search().props.placeholder, "Search my artifacts")
  assert.match(elementText(fixture.render()), /No artifacts match\./)
  search().props.onChange({ target: { value: "meal" } })
  assert.match(elementText(fixture.render()), /Meal swap prototype/)
})

test("history refreshes open lists on debounced updates and reloads on reopen", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const events = captureWindowListeners()
  let loads = 0
  try {
    const fixture = historyFixture({ fetchAIChatThreads: async () => { loads++; return [] } })
    fixture.open(); fixture.render(); await flush()
    for (let i = 0; i < 4; i++) events.emit("ai-chat-thread-updated", { threadId: "any" })
    t.mock.timers.tick(299)
    assert.equal(loads, 1)
    t.mock.timers.tick(1); await flush()
    assert.equal(loads, 2)
    findElement(fixture.render(), "Popover").props.onOpenChange(false); fixture.render()
    events.emit("ai-chat-thread-updated", { threadId: "any" }); t.mock.timers.tick(300)
    assert.equal(loads, 2)
    fixture.open(); await flush()
    assert.equal(loads, 3)
  } finally { events.restore() }
})

test("history regroups rows when an update arrives after midnight", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const events = captureWindowListeners()
  const clock = Date.now
  const evening = new Date(2026, 9, 15, 23, 50).getTime()
  try {
    Date.now = () => evening
    const fixture = historyFixture({ fetchAIChatThreads: async () => [{ id: "a", title: "Late chat", mode: "explore_idea", updatedAt: evening }] })
    fixture.open(); fixture.render(); await flush()
    assert.match(elementText(fixture.render()), /Today.*Late chat/s)
    Date.now = () => new Date(2026, 9, 16, 0, 10).getTime()
    events.emit("ai-chat-thread-updated", { threadId: "any" }); t.mock.timers.tick(300); await flush()
    const text = elementText(fixture.render())
    assert.match(text, /Last 7 days.*Late chat/s)
    assert.doesNotMatch(text, /Today/)
  } finally { Date.now = clock; events.restore() }
})

test("WS bursts resync only the open conversation and never replace a local stream", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const events = captureWindowListeners()
  let loads = 0, finish
  try {
    const fixture = chatFixture({ thread: "existing", api: {
      fetchAIChatThread: async () => { loads++; return { thread: thread("existing"), messages: [] } },
      streamAIChatTurn: () => new Promise((resolve) => { finish = resolve }),
    } })
    fixture.render(); fixture.requests[0].resolve(sources("A", ["A"])); await flush()
    events.emit("ai-chat-thread-updated", { threadId: "other" }); t.mock.timers.tick(300)
    assert.equal(loads, 1)
    for (let i = 0; i < 4; i++) events.emit("ai-chat-thread-updated", { threadId: "existing" })
    t.mock.timers.tick(299); assert.equal(loads, 1)
    t.mock.timers.tick(1); await flush(); assert.equal(loads, 2)
    // A turn starting during the debounce must suppress the queued sync too.
    events.emit("ai-chat-thread-updated", { threadId: "existing" })
    findElement(fixture.render(), "ChatComposer").props.onChange("Question")
    findElement(fixture.render(), "ChatComposer").props.onSend(); await flush()
    t.mock.timers.tick(300); assert.equal(loads, 2)
    events.emit("ai-chat-thread-updated", { threadId: "existing" }); t.mock.timers.tick(300)
    assert.equal(loads, 2)
    finish(); await flush(); assert.equal(loads, 3)
  } finally { events.restore() }
})

test("deleted artifact cards are neutral and expose no links or ticket actions", () => {
  const { ArtifactActionCard } = loadComponent("components/ai-chat/artifact-card.tsx", { react: createHooks().react, "lucide-react": {}, "@/components/ui/button": { Button: "Button" } })
  const tree = ArtifactActionCard({ block: { type: "action", kind: "artifact", state: "created", card: { deleted: true, title: "Prototype", slug: "prototype-abcdef", artifactId: "a" } }, ticket: { tracker: "Linear" }, onTicket() {} })
  assert.equal(elementText(tree), "This artifact was deleted.")
  assert.equal(findElement(tree, "Button"), undefined)
  assert.equal(findElement(tree, "a"), undefined)
})
