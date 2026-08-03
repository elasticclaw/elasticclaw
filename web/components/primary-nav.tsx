import { BarChart3, Bot } from "lucide-react"

// Primary routes rendered by the app-level header (components/app-header.tsx),
// which is the single place the navigation appears.
export const PRIMARY_NAV = [
  { href: "/", label: "Agents", icon: Bot },
  { href: "/analytics", label: "Analytics", icon: BarChart3 },
] as const

export function isPrimaryNavActive(pathname: string, href: string) {
  return href === "/" ? pathname === "/" : pathname.startsWith(href)
}
