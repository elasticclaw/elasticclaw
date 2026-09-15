"use client"

import { useEffect, useId, useState } from "react"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { fetchAgentOptions } from "@/lib/api"
import type { AgentConfig, AgentOptions } from "@/lib/agent-config"

export function AgentConfigForm({ value, onChange, disabled = false }: {
  value: AgentConfig
  onChange: (value: AgentConfig) => void
  disabled?: boolean
}) {
  const id = useId()
  const [options, setOptions] = useState<AgentOptions | null>(null)
  const [error, setError] = useState("")
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let active = true
    fetchAgentOptions().then(data => {
      if (active) { setOptions(data); setError("") }
    }).catch(e => { if (active) setError(e instanceof Error ? e.message : "Could not load credentials") })
    return () => { active = false }
  }, [attempt])

  const credentials = options?.credentials ?? []
  function credentialField(role: "main" | "subagents") {
    const selected = role === "main" ? value.llm_key : value.subagents?.llm_key
    return <div className="space-y-1.5">
      <label htmlFor={`${id}-${role}-credential`} className="text-xs font-medium">Credential</label>
      <select id={`${id}-${role}-credential`} className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm" value={selected ?? ""} disabled={disabled || !options}
        onChange={e => onChange(role === "main"
          ? { ...value, llm_key: e.target.value || undefined, default_model: credentials.find(c => c.name === e.target.value)?.default_model || undefined }
          : { ...value, subagents: { ...value.subagents, llm_key: e.target.value || undefined, model: undefined } })}>
        <option value="">{role === "main" ? "Use workflow or hub default" : "Use main agent credential"}</option>
        {selected && !credentials.some(c => c.name === selected) && <option value={selected}>{selected} (not listed)</option>}
        {credentials.map(c => <option key={c.name} value={c.name} disabled={!c.available}>{c.name} · {c.provider}{c.available ? "" : " (unavailable)"}</option>)}
      </select>
    </div>
  }
  return <div className="space-y-5">
    {error ? <div role="alert" className="text-sm text-destructive">{error} <Button type="button" variant="link" size="sm" onClick={() => setAttempt(n => n + 1)}>Retry</Button></div>
      : !options && <p role="status" className="text-xs text-muted-foreground">Loading credentials…</p>}
    <fieldset disabled={disabled} className="space-y-3">
      <legend className="mb-1 text-sm font-medium">Main agent</legend>
      <p className="text-xs text-muted-foreground">Plans the work, delegates tasks, and brings the results together.</p>
      {credentialField("main")}
      <div className="space-y-1.5">
        <label htmlFor={`${id}-main-model`} className="text-xs font-medium">Model</label>
        <Input id={`${id}-main-model`} value={value.default_model ?? ""} onChange={e => onChange({ ...value, default_model: e.target.value || undefined })} placeholder={credentials.find(c => c.name === value.llm_key)?.default_model || options?.default_model || "Use default model"} />
      </div>
    </fieldset>
    <fieldset disabled={disabled} className="space-y-3 border-t border-border pt-4">
      <legend className="text-sm font-medium">Subagents</legend>
      <p className="text-xs text-muted-foreground">Run delegated tasks. Choose another credential to use a different provider, or leave fields empty to inherit the main agent.</p>
      {credentialField("subagents")}
      <div className="space-y-1.5">
        <label htmlFor={`${id}-subagents-model`} className="text-xs font-medium">Model</label>
        <Input id={`${id}-subagents-model`} value={value.subagents?.model ?? ""} onChange={e => onChange({ ...value, subagents: { ...value.subagents, model: e.target.value || undefined } })} placeholder={credentials.find(c => c.name === value.subagents?.llm_key)?.default_model || (value.subagents?.llm_key ? "Use credential default model" : "Use main agent model")} />
      </div>
      <div className="space-y-1.5">
        <label htmlFor={`${id}-concurrency`} className="text-xs font-medium">Maximum concurrent subagents</label>
        <Input id={`${id}-concurrency`} type="number" min={1} step={1} value={value.subagents?.max_concurrent ?? ""} onChange={e => onChange({ ...value, subagents: { ...value.subagents, max_concurrent: e.target.value === "" ? undefined : Number(e.target.value) } })} placeholder="Use default limit" />
      </div>
    </fieldset>
  </div>
}
