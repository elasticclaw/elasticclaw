"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { BarChart3, Bot, History } from "lucide-react"
import { cn } from "@/lib/utils"

export const PRIMARY_NAV = [
  { href: "/", label: "Agents", icon: Bot },
  { href: "/runs", label: "Runs", icon: History },
  { href: "/analytics", label: "Analytics", icon: BarChart3 },
] as const

export function isPrimaryNavActive(pathname: string, href: string) {
  return href === "/" ? pathname === "/" : pathname.startsWith(href)
}

// Horizontal variant of the primary navigation for the standalone pages
// (/runs, /analytics) that do not mount the board sidebar, so navigation is
// two-way between all three routes and the active state renders everywhere.
export function PrimaryNavInline() {
  const pathname = usePathname()
  return (
    <nav className="flex items-center gap-1">
      {PRIMARY_NAV.map(({ href, label, icon: Icon }) => (
        <Link
          key={href}
          href={href}
          className={cn(
            "flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors",
            isPrimaryNavActive(pathname, href)
              ? "bg-accent text-foreground font-medium"
              : "text-muted-foreground hover:bg-accent hover:text-foreground"
          )}
        >
          <Icon className="size-4" />
          {label}
        </Link>
      ))}
    </nav>
  )
}
