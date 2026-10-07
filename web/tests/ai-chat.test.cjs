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
      useEffectEvent: (callback) => callback,
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
  assert.deepEqual(groups.map((group) => group.name), ["Knowledge", "Repositories", "Data", "Tracker", "Other"])
  assert.deepEqual(groups[2].sources.map((item) => item.kind), ["datadog", "posthog"])
  assert.equal(groups[4].sources[0].kind, "future")
  assert.deepEqual(sourceHelpers.groupSources([]), [])
})

function elementText(tree) {
  if (Array.isArray(tree)) return tree.map(elementText).join("")
  if (tree == null || typeof tree === "boolean") return ""
  if (typeof tree !== "object") return String(tree)
  return elementText(tree.props?.children)
}
function findElements(tree, type) {
  if (!tree || typeof tree !== "object") return []
  if (Array.isArray(tree)) return tree.flatMap((item) => findElements(item, type))
  return [...(tree.type === type ? [tree] : []), ...findElements(tree.props?.children, type)]
}

test("sources popover renders grouped details, read badges, errors, workspace, and refresh", () => {
  const { ChatHeader } = loadComponent("components/ai-chat/header.tsx", {
    "lucide-react": Object.fromEntries(["Activity", "BarChart3", "BookOpen", "ChevronDown", "Database", "GitBranch", "Plus", "RefreshCw", "Ticket", "TriangleAlert"].map((name) => [name, name])),
    "@/components/ui/button": { Button: "Button" },
    "@/components/ui/popover": { Popover: "Popover", PopoverContent: "PopoverContent", PopoverTrigger: "PopoverTrigger" },
    "./mode-cards": { chatModes: [] },
    "./sources": sourceHelpers,
  })
  let refreshed = 0
  const props = { mode: null, onNewChat() {}, onRefreshSources() { refreshed++ }, data: sources("fasterway", ["fasterway"], { sources: [
    source("knowledge_base", "connected", { name: "Knowledge base", detail: "owner/repo · 61 pages" }),
    source("repositories", "connected", { name: "Repositories", detail: "a, b, c · default branch" }),
    source("posthog", "connected", { name: "PostHog", detail: "Project Web app · events and saved insights" }),
    source("datadog", "unreachable", { name: "Datadog", error: "The API key was rejected." }),
    source("issue_tracker", "invalid", { name: "Linear", error: "Choose a team." }),
    source("__proto__", "future-status", { name: "Future provider" }),
  ] }) }
  const tree = ChatHeader(props)
  const text = elementText(tree)
  for (const expected of ["What this chat can read", "Workspace fasterway", "The assistant reads these and never changes them.", "owner/repo · 61 pages", "3 of 6 sources", "Connected", "Can't reach", "The API key was rejected.", "Invalid", "Choose a team.", "Future provider", "Unknown", "Set in the workspace configuration, the same for everyone in this workspace."]) assert.ok(text.includes(expected), expected)
  assert.doesNotMatch(text, /create artifacts|create ticket|unchecked/i)
  assert.equal(findElements(tree, "span").filter((item) => elementText(item) === "Read").length, 6)
  assert.equal(findElements(tree, "h3").map(elementText).join(","), "Knowledge,Repositories,Data,Tracker,Other")
  assert.ok(findElements(tree, "span").some((item) => elementText(item) === "owner/repo" && item.props.className.includes("font-mono")))
  assert.ok(findElement(tree, "Database"))
  const refresh = findElements(tree, "Button").find((item) => elementText(item) === "Refresh")
  refresh.props.onClick()
  assert.equal(refreshed, 1)
  const loading = ChatHeader({ ...props, loading: true })
  assert.equal(findElements(loading, "Button").find((item) => elementText(item) === "Refresh").props.disabled, true)
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
