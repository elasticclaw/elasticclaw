const assert = require("node:assert/strict")
const { readFileSync } = require("node:fs")
const path = require("node:path")
const test = require("node:test")
const ts = require("typescript")

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
            effects[index] = { deps, cleanup: callback() }
          })
        }
      },
      useRef: (current) => ({ current }),
      useCallback: (callback) => callback,
      useSyncExternalStore: (_subscribe, getSnapshot) => getSnapshot(),
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

function chatFixture() {
  const hooks = createHooks()
  const requests = []
  const { ChatScreen } = loadComponent("components/ai-chat/chat-screen.tsx", {
    react: hooks.react,
    "next/navigation": { useRouter: () => ({ replace() {} }) },
    "lucide-react": { Sparkles: "Sparkles" },
    "@/hooks/use-feature-flag": { useFeatureFlag: () => true, useFeatureFlagsLoaded: () => true },
    "@/lib/api": {
      fetchAIChatSources: (workspace) => new Promise((resolve) => requests.push({ workspace, resolve })),
    },
    "./header": { ChatHeader: "ChatHeader" },
  })
  const component = hooks.render(ChatScreen).type
  return { requests, render: () => hooks.render(component) }
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
