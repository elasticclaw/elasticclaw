"use client"

import { useState, useEffect, useCallback } from "react"
import { useRouter } from "next/navigation"
import { Zap, Play, AlertCircle, Factory as FactoryIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"
import { fetchFactories, triggerFactory, type Factory, type FactoryInput } from "@/lib/api"

interface ManualTriggerPanelProps {
  className?: string
}

export function ManualTriggerPanel({ className }: ManualTriggerPanelProps) {
  const [factories, setFactories] = useState<Factory[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selectedFactory, setSelectedFactory] = useState<Factory | null>(null)
  const [inputValues, setInputValues] = useState<Record<string, string>>({})
  const [triggering, setTriggering] = useState(false)
  const [triggerError, setTriggerError] = useState<string | null>(null)
  const [showConfirm, setShowConfirm] = useState(false)
  const router = useRouter()

  // Load factories on mount
  useEffect(() => {
    let cancelled = false
    fetchFactories()
      .then((data) => {
        if (cancelled) return
        // Filter to enabled factories with manual trigger enabled.
        // Enabled is *bool on the backend (nil = enabled by default), so
        // treat undefined/null as true.
        const manualFactories = data.filter(
          (f) => (f.enabled !== false) && f.enableManualTrigger
        )
        setFactories(manualFactories)
        setLoading(false)
      })
      .catch((err) => {
        if (cancelled) return
        setError(String(err))
        setLoading(false)
      })
    return () => { cancelled = true }
  }, [])

  // Reset input values when selecting a different factory
  useEffect(() => {
    if (selectedFactory?.inputs) {
      const defaults: Record<string, string> = {}
      for (const input of selectedFactory.inputs) {
        if (input.default) {
          defaults[input.name] = input.default
        }
      }
      setInputValues(defaults)
    } else {
      setInputValues({})
    }
    setTriggerError(null)
    setShowConfirm(false)
  }, [selectedFactory])

  const handleTrigger = useCallback(async () => {
    if (!selectedFactory) return
    setTriggering(true)
    setTriggerError(null)
    try {
      const body: Record<string, unknown> = {}
      if (selectedFactory.inputs && selectedFactory.inputs.length > 0) {
        const inputs: Record<string, unknown> = {}
        for (const input of selectedFactory.inputs) {
          const val = inputValues[input.name]
          if (input.type === "bool") {
            inputs[input.name] = val === "true"
          } else if (input.type === "number") {
            inputs[input.name] = parseFloat(val || "0")
          } else {
            inputs[input.name] = val || ""
          }
        }
        body.inputs = inputs
      }
      const res = await triggerFactory(selectedFactory.name, body.inputs as Record<string, unknown>)
      // Navigate to the new claw using Next.js router (preserves SPA state)
      router.push(`/claws/${res.claw_id}`)
    } catch (e) {
      setTriggerError(String(e))
      setTriggering(false)
    }
  }, [selectedFactory, inputValues])

  // Filter to factories that have manual trigger enabled
  const triggerableFactories = factories

  if (loading) {
    return (
      <div className={cn("rounded-lg border border-border bg-card p-4", className)}>
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <FactoryIcon className="size-4" />
          <span>Loading factories...</span>
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className={cn("rounded-lg border border-border bg-card p-4", className)}>
        <div className="flex items-center gap-2 text-sm text-red-500">
          <AlertCircle className="size-4" />
          <span>Failed to load factories: {error}</span>
        </div>
      </div>
    )
  }

  if (triggerableFactories.length === 0) {
    return null
  }

  return (
    <div className={cn("rounded-lg border border-border bg-card", className)}>
      <div className="flex items-center gap-2 px-4 py-3 border-b border-border">
        <Zap className="size-4 text-amber-500" />
        <h3 className="text-sm font-medium">Manual Triggers</h3>
        <Badge variant="secondary" className="text-xs">
          {triggerableFactories.length}
        </Badge>
      </div>

      <div className="p-4 space-y-4">
        {/* Factory selector */}
        <div className="space-y-1.5">
          <label className="text-xs font-medium text-muted-foreground">Factory</label>
          <select
            className="w-full text-sm border border-border rounded-md px-3 py-2 bg-background"
            value={selectedFactory?.name || ""}
            onChange={(e) => {
              const f = triggerableFactories.find((f) => f.name === e.target.value)
              setSelectedFactory(f || null)
            }}
          >
            <option value="">Select a factory...</option>
            {triggerableFactories.map((f) => (
              <option key={f.name} value={f.name}>
                {f.name} ({f.template})
              </option>
            ))}
          </select>
        </div>

        {/* Input form */}
        {selectedFactory && selectedFactory.inputs && selectedFactory.inputs.length > 0 && (
          <div className="space-y-3">
            <div className="text-xs font-medium text-muted-foreground">Inputs</div>
            {selectedFactory.inputs.map((input) => (
              <FactoryInputField
                key={input.name}
                input={input}
                value={inputValues[input.name] || ""}
                onChange={(val) => setInputValues((prev) => ({ ...prev, [input.name]: val }))}
              />
            ))}
          </div>
        )}

        {/* Confirm + Trigger */}
        {selectedFactory && (
          <div className="space-y-3">
            {!showConfirm ? (
              <Button
                size="sm"
                className="w-full"
                onClick={() => setShowConfirm(true)}
              >
                <Play className="size-4 mr-1" />
                Trigger {selectedFactory.name}
              </Button>
            ) : (
              <div className="space-y-2">
                <p className="text-xs text-muted-foreground text-center">
                  Are you sure you want to trigger <strong>{selectedFactory.name}</strong>?
                </p>
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="ghost"
                    className="flex-1"
                    onClick={() => setShowConfirm(false)}
                    disabled={triggering}
                  >
                    Cancel
                  </Button>
                  <Button
                    size="sm"
                    className="flex-1"
                    onClick={handleTrigger}
                    disabled={triggering}
                  >
                    {triggering ? "Triggering..." : "Confirm"}
                  </Button>
                </div>
              </div>
            )}
            {triggerError && (
              <div className="flex items-center gap-1.5 text-xs text-red-500">
                <AlertCircle className="size-3" />
                <span>{triggerError}</span>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function FactoryInputField({
  input,
  value,
  onChange,
}: {
  input: FactoryInput
  value: string
  onChange: (val: string) => void
}) {
  return (
    <div className="space-y-1">
      <label className="text-xs font-medium">
        {input.name}
        {input.required && <span className="text-red-500 ml-0.5">*</span>}
      </label>
      {input.description && (
        <p className="text-xs text-muted-foreground">{input.description}</p>
      )}
      {input.type === "enum" && input.options ? (
        <select
          className="w-full text-sm border border-border rounded-md px-3 py-2 bg-background"
          value={value || input.default || ""}
          onChange={(e) => onChange(e.target.value)}
        >
          {input.options.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      ) : input.type === "bool" ? (
        <select
          className="w-full text-sm border border-border rounded-md px-3 py-2 bg-background"
          value={value || input.default || "false"}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      ) : (
        <Input
          type={input.type === "number" ? "number" : "text"}
          value={value || ""}
          onChange={(e) => onChange(e.target.value)}
          placeholder={input.default}
          className="h-9"
        />
      )}
    </div>
  )
}
