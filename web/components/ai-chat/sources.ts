import type { AIChatSource } from "@/lib/api"

export function sourceStatus(status: string) {
  switch (status) {
    case "connected": return { label: "Connected", dotClass: "bg-green-500", textClass: "text-muted-foreground", warning: false }
    case "unreachable": return { label: "Can't reach", dotClass: "bg-amber-500", textClass: "text-amber-600 dark:text-amber-400", warning: true }
    case "invalid": return { label: "Invalid", dotClass: "bg-destructive", textClass: "text-destructive", warning: true }
    default: return { label: "Unknown", dotClass: "bg-muted-foreground", textClass: "text-muted-foreground", warning: false }
  }
}

export function sourceSummary(sources: AIChatSource[], loading = false) {
  const connected = sources.filter((source) => source.status === "connected").length
  const allConnected = connected === sources.length
  return {
    label: allConnected ? `${sources.length} sources` : `${connected} of ${sources.length} sources`,
    dotClass: loading || sources.length === 0 ? "bg-muted-foreground" : allConnected ? "bg-green-500" : "bg-amber-500",
  }
}

export function groupSources(sources: AIChatSource[]) {
  const groups = [
    { name: "Knowledge", kinds: ["knowledge_base", "repositories"] },
    { name: "Data", kinds: ["posthog", "datadog"] },
    { name: "Tracker", kinds: ["issue_tracker"] },
  ]
  const knownKinds = groups.flatMap((group) => group.kinds)
  return [
    ...groups.map((group) => ({ name: group.name, sources: group.kinds.flatMap((kind) => sources.filter((source) => source.kind === kind)) })),
    { name: "Other", sources: sources.filter((source) => !knownKinds.includes(source.kind)) },
  ].filter((group) => group.sources.length > 0)
}

// Repository lists drop the shared owner prefix, as in the design, unless that
// would make two names ambiguous.
export function repositoryNames(kind: string, names: string) {
  if (kind !== "repositories") return names
  const repos = names.split(", ")
  const short = repos.map((repo) => repo.slice(repo.indexOf("/") + 1))
  return new Set(short).size === short.length ? short.join(", ") : names
}

// Sources left out of a turn say so, since the runner drops their tools.
export function skipNotice(source: AIChatSource) {
  return source.status === "unreachable" || source.status === "invalid" ? `Answers skip ${source.name || "this source"} until this is fixed.` : ""
}
