import assert from "node:assert/strict"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"
import ts from "typescript"

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))

// Run the pure timeline code without adding a browser test dependency.
const directory = fs.mkdtempSync(path.join(os.tmpdir(), "elasticclaw-subagent-test-"))
try {
  for (const name of ["turns", "subagents"]) {
    const source = fs.readFileSync(path.join(scriptDirectory, "../lib", `${name}.ts`), "utf8")
    fs.writeFileSync(path.join(directory, `${name}.js`), ts.transpileModule(source, {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
    }).outputText)
  }
  const { default: { currentTurnSubagents, subagentCounts, subagentSummaryLine } } = await import(pathToFileURL(path.join(directory, "subagents.js")).href)
  const now = Date.now()
  const user = { id: "user", role: "user", content: "Delegate work", timestamp: new Date(now - 2000) }
  const event = (id, phase, extra = {}) => ({
    id, role: "activity", content: "", timestamp: new Date(now - (phase === "start" ? 1000 : 500)),
    activity: { kind: "tool", tool: "sessions_spawn", phase, call_id: "spawn-1", ...extra },
  })
  const start = event("start", "start", { subagent_requested_model: "openai/worker", subagent_prompt: "Inspect API" })
  const accepted = event("receipt", "result", { subagent_spawn_status: "accepted", subagent_child_session: "child", subagent_child_run: "run", result: '{"status":"accepted"}' })
  const launched = currentTurnSubagents([user, start, accepted], now)
  assert.equal(launched.length, 1, "start and receipt must represent one child")
  assert.equal(launched[0].status, "launched")
  assert.equal(launched[0].endedAt, undefined, "spawn receipt does not end the child")
  assert.equal(launched[0].durationMs, undefined, "spawn duration is not child duration")
  assert.equal(launched[0].requestedModel, "openai/worker")
  assert.equal(launched[0].resolvedModel, undefined, "requested model is not actual model")
  assert.equal(launched[0].childSession, "child")
  assert.equal(subagentCounts(launched).running, 0)
  assert.equal(subagentCounts(launched).done, 0)
  assert.match(subagentSummaryLine(launched), /launched/)
  const failed = currentTurnSubagents([user, start, event("failure", "result", { subagent_spawn_status: "failed", error: "Forbidden" })], now)
  assert.equal(failed[0].status, "failed")
  const unknown = currentTurnSubagents([user, start, event("unknown", "result")], now)
  assert.equal(unknown[0].status, "unknown")
  assert.doesNotMatch(subagentSummaryLine(unknown), /done|running/)
  const task = [start, accepted].map(message => ({ ...message, activity: { ...message.activity, tool: "Task", subagent_spawn_status: undefined } }))
  assert.equal(currentTurnSubagents([user, ...task], now)[0].status, "done", "synchronous Task behavior is unchanged")
  console.log("Subagent scheduling and completion tests passed")
} finally {
  fs.rmSync(directory, { recursive: true, force: true })
}
