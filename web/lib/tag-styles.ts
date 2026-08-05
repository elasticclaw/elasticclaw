export function tagBadgeClass(tag: string): string {
  if (tag === "routine") {
    return "border border-violet-400/40 bg-violet-500/20 text-violet-300"
  }
  return "bg-secondary text-muted-foreground"
}
