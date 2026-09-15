"use client"

import { useEffect, useId, useState } from "react"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { fetchAgentOptions } from "@/lib/api"
import type { AgentConfig, AgentOptions } from "@/lib/agent-config"

export function AgentConfigForm({
  value,
  onChange,
  disabled = false,
}: {
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
    fetchAgentOptions()
      .then(data => {
        if (active) {
          setOptions(data)
          setError("")
        }
      })
      .catch(e => {
        if (active) setError(e instanceof Error ? e.message : "Could not load credentials")
      })
    return () => { active = false }
  }, [attempt])

  const credentials = options?.credentials ?? []
  const inheritsSubagents = value.subagents === undefined
  const mainModelPlaceholder = credentials.find(c => c.name === value.llm_key)?.default_model
    || options?.default_model
    || "Use default model"
  const subagentModelPlaceholder = inheritsSubagents
    ? "Use inherited settings"
    : credentials.find(c => c.name === value.subagents?.llm_key)?.default_model
      || (value.subagents?.llm_key ? "Use credential default model" : "Use main agent model")

  function changeCredential(role: "main" | "subagents", name: string) {
    if (role === "main") {
      onChange({
        ...value,
        llm_key: name || undefined,
        default_model: credentials.find(c => c.name === name)?.default_model || undefined,
      })
    } else {
      onChange({
        ...value,
        subagents: { ...value.subagents, llm_key: name || undefined, model: undefined },
      })
    }
  }

  function useMainAgent() {
    const maxConcurrent = value.subagents?.max_concurrent
    onChange({
      ...value,
      subagents: maxConcurrent === undefined ? {} : { max_concurrent: maxConcurrent },
    })
  }

  function credentialField(role: "main" | "subagents") {
    const selected = role === "main" ? value.llm_key : value.subagents?.llm_key
    const defaultLabel = role === "main"
      ? "Use workflow or hub default"
      : inheritsSubagents ? "Use inherited settings" : "Use main agent credential"

    return (
      <div className="space-y-1.5">
        <label htmlFor={`${id}-${role}-credential`} className="text-xs font-medium">
          Credential
        </label>
        <select
          id={`${id}-${role}-credential`}
          className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
          value={selected ?? ""}
          disabled={disabled || !options}
          onChange={e => changeCredential(role, e.target.value)}
        >
          <option value="">{defaultLabel}</option>
          {selected && !credentials.some(c => c.name === selected) && (
            <option value={selected}>{selected} (not listed)</option>
          )}
          {credentials.map(credential => (
            <option key={credential.name} value={credential.name} disabled={!credential.available}>
              {credential.name} · {credential.provider}{credential.available ? "" : " (unavailable)"}
            </option>
          ))}
        </select>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      {error ? (
        <div role="alert" className="text-sm text-destructive">
          {error}
          <Button type="button" variant="link" size="sm" onClick={() => setAttempt(n => n + 1)}>
            Retry
          </Button>
        </div>
      ) : !options && (
        <p role="status" className="text-xs text-muted-foreground">Loading credentials…</p>
      )}

      <fieldset disabled={disabled} className="space-y-3">
        <legend className="mb-1 text-sm font-medium">Main agent</legend>
        <p className="text-xs text-muted-foreground">
          Plans the work, delegates tasks, and brings the results together.
        </p>
        {credentialField("main")}
        <div className="space-y-1.5">
          <label htmlFor={`${id}-main-model`} className="text-xs font-medium">Model</label>
          <Input
            id={`${id}-main-model`}
            value={value.default_model ?? ""}
            onChange={e => onChange({ ...value, default_model: e.target.value || undefined })}
            placeholder={mainModelPlaceholder}
          />
        </div>
      </fieldset>

      <fieldset disabled={disabled} className="space-y-3 border-t border-border pt-4">
        <legend className="text-sm font-medium">Subagents</legend>
        <div className="flex items-start justify-between gap-3">
          <p className="text-xs text-muted-foreground">
            {inheritsSubagents
              ? "Uses inherited workflow or template settings. Choose Use main agent to override them."
              : "Overrides inherited subagent settings. Empty model and credential fields use the main agent."}
          </p>
          <Button type="button" variant="outline" size="sm" onClick={useMainAgent}>
            Use main agent
          </Button>
        </div>
        {credentialField("subagents")}
        <div className="space-y-1.5">
          <label htmlFor={`${id}-subagents-model`} className="text-xs font-medium">Model</label>
          <Input
            id={`${id}-subagents-model`}
            value={value.subagents?.model ?? ""}
            onChange={e => onChange({
              ...value,
              subagents: { ...value.subagents, model: e.target.value || undefined },
            })}
            placeholder={subagentModelPlaceholder}
          />
        </div>
        <div className="space-y-1.5">
          <label htmlFor={`${id}-concurrency`} className="text-xs font-medium">
            Maximum concurrent subagents
          </label>
          <Input
            id={`${id}-concurrency`}
            type="number"
            min={1}
            max={32}
            step={1}
            value={value.subagents?.max_concurrent ?? ""}
            onChange={e => onChange({
              ...value,
              subagents: {
                ...value.subagents,
                max_concurrent: e.target.value === "" ? undefined : Number(e.target.value),
              },
            })}
            placeholder="Use default limit"
          />
        </div>
      </fieldset>
    </div>
  )
}
