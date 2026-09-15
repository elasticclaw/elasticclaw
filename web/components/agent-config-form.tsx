"use client"

import { useEffect, useId, useRef, useState } from "react"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { fetchAgentOptions } from "@/lib/api"
import { validateAgentConfig, type AgentConfig, type AgentOptions } from "@/lib/agent-config"

export function AgentConfigForm({
  value,
  onChange,
  disabled = false,
  context = "workflow",
}: {
  value: AgentConfig
  onChange: (value: AgentConfig) => void
  disabled?: boolean
  context?: "workflow" | "run"
}) {
  const id = useId()
  const [options, setOptions] = useState<AgentOptions | null>(null)
  const [error, setError] = useState("")
  const [attempt, setAttempt] = useState(0)
  const mainCredentialRef = useRef<HTMLSelectElement>(null)
  const retryButtonRef = useRef<HTMLButtonElement>(null)
  const restoreRetryFocus = useRef(false)

  useEffect(() => {
    let active = true
    fetchAgentOptions()
      .then(data => {
        if (active) {
          restoreRetryFocus.current = document.activeElement === retryButtonRef.current
          setOptions(data)
          setError("")
        }
      })
      .catch(e => {
        if (active) setError(e instanceof Error ? e.message : "Could not load credentials")
      })
    return () => { active = false }
  }, [attempt])

  useEffect(() => {
    if (options && restoreRetryFocus.current) {
      restoreRetryFocus.current = false
      mainCredentialRef.current?.focus()
    }
  }, [options])

  const concurrencyError = validateAgentConfig(value)
  const credentials = options?.credentials ?? []
  const inheritsSubagents = value.subagents === undefined
  const mainModelPlaceholder = value.llm_key
    ? credentials.find(c => c.name === value.llm_key)?.default_model || "Use credential default model"
    : context === "run" ? "Use workflow model" : "Use inherited model"
  const usesMainCredential = !value.subagents?.llm_key || value.subagents.llm_key === value.llm_key
  const subagentModelPlaceholder = inheritsSubagents
    ? "Use inherited model"
    : usesMainCredential
      ? "Use main agent model"
      : value.llm_key
        ? credentials.find(c => c.name === value.subagents?.llm_key)?.default_model || "Use credential default model"
        : "Use default model"

  function changeCredential(role: "main" | "subagents", name: string) {
    if (role === "main") {
      onChange({
        ...value,
        llm_key: name || undefined,
        default_model: undefined,
      })
    } else {
      onChange({
        ...value,
        subagents: { ...value.subagents, llm_key: name || undefined, model: undefined },
      })
    }
  }

  function restoreInheritedSettings() {
    const inherited = { ...value }
    delete inherited.subagents
    onChange(inherited)
  }

  function selectMainAgent() {
    const maxConcurrent = value.subagents?.max_concurrent
    onChange({
      ...value,
      subagents: maxConcurrent === undefined ? {} : { max_concurrent: maxConcurrent },
    })
  }

  function credentialField(role: "main" | "subagents") {
    const selected = role === "main" ? value.llm_key : value.subagents?.llm_key
    const defaultLabel = role === "main"
      ? context === "run" ? "Use workflow credential" : "Use inherited credential"
      : inheritsSubagents ? "Use inherited settings" : "Use main agent credential"

    return (
      <div className="space-y-1.5">
        <label htmlFor={`${id}-${role}-credential`} className="text-xs font-medium">
          Credential
        </label>
        <select
          ref={role === "main" ? mainCredentialRef : undefined}
          id={`${id}-${role}-credential`}
          className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
          value={selected ?? ""}
          disabled={disabled || !options}
          onChange={e => changeCredential(role, e.target.value)}
        >
          <option value="">{defaultLabel}</option>
          {selected && !credentials.some(c => c.name === selected) && (
            <option value={selected}>{selected}{options ? " (not listed)" : ""}</option>
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
      {(error || (!options && attempt > 0)) && (
        <div className="text-sm text-destructive">
          {error && <p role="alert">{error}</p>}
          <Button
            ref={retryButtonRef}
            type="button"
            variant="link"
            size="sm"
            aria-disabled={!error}
            onClick={() => {
              if (!error) return
              setError("")
              setAttempt(n => n + 1)
            }}
          >
            Retry
          </Button>
        </div>
      )}
      {!options && !error && (
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
        <div className="space-y-2">
          <p className="text-xs text-muted-foreground">
            {inheritsSubagents
              ? "Uses inherited workflow or template settings. Customizing replaces the entire inherited subagent configuration, starting with the main agent’s model and credential and the default concurrency limit."
              : "These settings replace the inherited subagent configuration, including its concurrency limit."}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-expanded={!inheritsSubagents}
              aria-controls={`${id}-subagent-fields`}
              onClick={() => inheritsSubagents
                ? onChange({ ...value, subagents: {} })
                : restoreInheritedSettings()}
            >
              {inheritsSubagents ? "Customize subagents" : "Use inherited settings"}
            </Button>
            {!inheritsSubagents && (
              <Button type="button" variant="outline" size="sm" onClick={selectMainAgent}>
                Use main agent
              </Button>
            )}
          </div>
        </div>
        <div id={`${id}-subagent-fields`} hidden={inheritsSubagents}>
          {!inheritsSubagents && (
            <div className="space-y-3">
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
                  aria-describedby={`${id}-subagents-model-help`}
                />
                <p id={`${id}-subagents-model-help`} className="text-xs text-muted-foreground">
                  Leave the model empty to use the main agent’s model with the same credential,
                  or the credential’s default model with a different credential.
                </p>
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
                  aria-invalid={concurrencyError ? true : undefined}
                  aria-describedby={concurrencyError ? `${id}-concurrency-error` : undefined}
                  placeholder="Use default limit"
                />
                {concurrencyError && (
                  <p id={`${id}-concurrency-error`} className="text-xs text-destructive">
                    {concurrencyError}
                  </p>
                )}
              </div>
            </div>
          )}
        </div>
      </fieldset>
    </div>
  )
}
