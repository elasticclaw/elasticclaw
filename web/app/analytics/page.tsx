"use client"

import { HomeShell } from "@/components/home-shell"

// Deep link into the analytics view of the same shell rendered at "/".
// HomeShell derives the view from the pathname, so this static-export route
// only needs to hydrate the shared component.
export default function AnalyticsPage() {
  return <HomeShell />
}
