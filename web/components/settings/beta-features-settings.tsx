"use client"

import { useEffect, useState, type FormEvent } from "react"
import { Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"
import {
  addBetaTester,
  getFeatureFlags,
  removeBetaTester,
  updateFeatureFlagStage,
  type BetaTester,
  type FeatureFlag,
  type FeatureStage,
} from "@/lib/api"
import { refreshFeatureFlags } from "@/hooks/use-feature-flag"

const STAGES: { value: FeatureStage; label: string; activeClass: string }[] = [
  { value: "off", label: "Off", activeClass: "bg-secondary text-foreground" },
  { value: "beta", label: "Beta", activeClass: "bg-amber-500/20 text-amber-500" },
  { value: "on", label: "On", activeClass: "bg-green-500/20 text-green-400" },
]

function errorMessage(e: unknown, fallback: string): string {
  return e instanceof Error && e.message ? e.message : fallback
}

function byLogin(a: BetaTester, b: BetaTester): number {
  return a.login.localeCompare(b.login)
}

function withItem(set: ReadonlySet<string>, item: string, present: boolean): ReadonlySet<string> {
  const next = new Set(set)
  if (present) next.add(item)
  else next.delete(item)
  return next
}

function formatAddedAt(addedAt: number): string {
  if (!addedAt) return ""
  return new Date(addedAt).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })
}

export function BetaFeaturesSettings() {
  const [flags, setFlags] = useState<FeatureFlag[] | null>(null)
  const [testers, setTesters] = useState<BetaTester[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)

  const [savingKeys, setSavingKeys] = useState<ReadonlySet<string>>(new Set())
  const [flagErrors, setFlagErrors] = useState<Record<string, string>>({})

  const [newLogin, setNewLogin] = useState("")
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState<ReadonlySet<string>>(new Set())
  const [testerError, setTesterError] = useState<string | null>(null)

  useEffect(() => {
    getFeatureFlags()
      .then((data) => {
        setFlags(data.flags ?? [])
        setTesters([...(data.beta_testers ?? [])].sort(byLogin))
      })
      .catch((e) => setLoadError(errorMessage(e, "Failed to load feature flags")))
  }, [])

  const setFlagError = (key: string, message: string | null) => {
    setFlagErrors((current) => {
      const next = { ...current }
      if (message) next[key] = message
      else delete next[key]
      return next
    })
  }

  const handleStageChange = async (flag: FeatureFlag, stage: FeatureStage) => {
    if (stage === flag.stage || savingKeys.has(flag.key)) return
    const previous = flag.stage
    const setStage = (value: FeatureStage) =>
      setFlags((current) => current?.map((f) => (f.key === flag.key ? { ...f, stage: value } : f)) ?? current)

    setStage(stage)
    setSavingKeys((current) => withItem(current, flag.key, true))
    setFlagError(flag.key, null)
    try {
      const updated = await updateFeatureFlagStage(flag.key, stage)
      setFlags((current) => current?.map((f) => (f.key === flag.key ? updated : f)) ?? current)
      refreshFeatureFlags()
    } catch (e) {
      setStage(previous)
      setFlagError(flag.key, errorMessage(e, "Could not change the stage"))
    } finally {
      setSavingKeys((current) => withItem(current, flag.key, false))
    }
  }

  const handleAdd = async (event: FormEvent) => {
    event.preventDefault()
    const login = newLogin.trim().replace(/^@/, "")
    if (!login) return
    setAdding(true)
    setTesterError(null)
    try {
      const tester = await addBetaTester(login)
      setTesters((current) => [...current.filter((t) => t.login !== tester.login), tester].sort(byLogin))
      setNewLogin("")
      refreshFeatureFlags()
    } catch (e) {
      setTesterError(errorMessage(e, "Could not add the tester"))
    } finally {
      setAdding(false)
    }
  }

  const handleRemove = async (login: string) => {
    setRemoving((current) => withItem(current, login, true))
    setTesterError(null)
    try {
      await removeBetaTester(login)
      setTesters((current) => current.filter((t) => t.login !== login))
      refreshFeatureFlags()
    } catch (e) {
      setTesterError(errorMessage(e, `Could not remove ${login}`))
    } finally {
      setRemoving((current) => withItem(current, login, false))
    }
  }

  return (
    <div className="space-y-8">
      <div>
        <h2 className="text-base font-semibold mb-1">Beta features</h2>
        <p className="text-sm text-muted-foreground">
          Roll out features in stages. Beta turns a flag on only for the testers below.
        </p>
      </div>

      {flags === null ? (
        loadError ? (
          <div className="rounded-lg border border-red-500/20 bg-red-500/5 p-4 text-sm text-red-500">
            {loadError}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground animate-pulse">Loading…</p>
        )
      ) : (
        <>
          <section className="space-y-3">
            <h3 className="text-sm font-medium">Flags</h3>
            <div className="border border-border rounded-lg divide-y divide-border">
              {flags.length === 0 ? (
                <div className="px-4 py-6 text-center">
                  <p className="text-sm text-muted-foreground">No feature flags yet.</p>
                  <p className="text-xs text-muted-foreground mt-1">
                    Flags are declared in <code className="bg-muted px-1 rounded font-mono">pkg/hub/feature_flags.go</code>.
                  </p>
                </div>
              ) : (
                flags.map((flag) => (
                  <div key={flag.key} className="flex items-start justify-between gap-4 px-4 py-3">
                    <div className="min-w-0">
                      <div className="flex items-baseline gap-2 flex-wrap">
                        <span className="text-sm font-medium">{flag.name}</span>
                        <code className="text-xs font-mono text-muted-foreground">{flag.key}</code>
                      </div>
                      {flag.description && (
                        <p className="text-xs text-muted-foreground mt-0.5">{flag.description}</p>
                      )}
                      {flagErrors[flag.key] && (
                        <p className="text-xs text-destructive mt-1">{flagErrors[flag.key]}</p>
                      )}
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      {flag.stage === flag.default_stage && (
                        <span className="text-[11px] text-muted-foreground">default</span>
                      )}
                      <div
                        role="group"
                        aria-label={`${flag.name} stage`}
                        className="flex items-center gap-0.5 rounded-md border border-border bg-muted/30 p-0.5"
                      >
                        {STAGES.map((option) => {
                          const active = flag.stage === option.value
                          return (
                            <button
                              key={option.value}
                              type="button"
                              aria-pressed={active}
                              disabled={savingKeys.has(flag.key)}
                              onClick={() => handleStageChange(flag, option.value)}
                              className={cn(
                                "rounded px-2.5 py-1 text-xs transition-colors disabled:cursor-wait",
                                active
                                  ? cn("font-medium", option.activeClass)
                                  : "text-muted-foreground hover:text-foreground"
                              )}
                            >
                              {option.label}
                            </button>
                          )
                        })}
                      </div>
                    </div>
                  </div>
                ))
              )}
            </div>
          </section>

          <section className="space-y-3">
            <div>
              <h3 className="text-sm font-medium">Beta testers</h3>
              <p className="text-xs text-muted-foreground mt-0.5">
                GitHub users who get every flag in Beta. Admins are not included automatically.
              </p>
            </div>

            <div className="border border-border rounded-lg divide-y divide-border">
              {testers.length === 0 ? (
                <p className="text-sm text-muted-foreground px-4 py-6 text-center">No beta testers yet.</p>
              ) : (
                testers.map((tester) => {
                  const addedAt = formatAddedAt(tester.added_at)
                  return (
                    <div key={tester.login} className="flex items-center justify-between gap-4 px-4 py-2.5">
                      <div className="min-w-0">
                        <p className="text-sm font-mono truncate">{tester.login}</p>
                        {(tester.added_by || addedAt) && (
                          <p className="text-xs text-muted-foreground mt-0.5">
                            Added{tester.added_by && ` by ${tester.added_by}`}{addedAt && ` · ${addedAt}`}
                          </p>
                        )}
                      </div>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="text-muted-foreground hover:text-destructive"
                        aria-label={`Remove ${tester.login}`}
                        disabled={removing.has(tester.login)}
                        onClick={() => handleRemove(tester.login)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  )
                })
              )}
            </div>

            <form onSubmit={handleAdd} className="flex gap-2 max-w-md">
              <Input
                value={newLogin}
                onChange={(e) => setNewLogin(e.target.value)}
                placeholder="GitHub username"
                aria-label="GitHub username"
                autoComplete="off"
                spellCheck={false}
                className="h-8 text-sm font-mono"
              />
              <Button type="submit" size="sm" disabled={adding || !newLogin.trim().replace(/^@/, "")}>
                {adding ? "Adding…" : "Add"}
              </Button>
            </form>
            {testerError && <p className="text-xs text-destructive">{testerError}</p>}
          </section>
        </>
      )}
    </div>
  )
}
