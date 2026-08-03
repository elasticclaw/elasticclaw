import { Suspense } from "react"
import SettingsSectionPage from "./settings-content"
import { LEGACY_ANALYTICS_SECTION, VALID_SECTIONS } from "./sections"

/**
 * Static export path generation for the settings route.
 *
 * Supported URL shapes at runtime (handled client-side in SettingsSectionPage):
 *   /settings                           → root, defaults to workspaces section
 *   /settings/{section}                 → system section (runtimes, models, github, etc.)
 *   /settings/{workspace}/{section}     → workspace-scoped section (workspaces, workflows, github, etc.)
 *   /settings/{workspace}               → workspace overview (redirects to /{workspace}/workspaces)
 *
 * Because workspace names are dynamic (loaded from API), we cannot pre-generate
 * all /settings/{workspace}/{section} combinations at build time. Instead, we:
 *   1. Generate all system-section paths (no workspace)
 *   2. Generate a "_workspace" placeholder that the client uses as a sentinel
 *      to select the first available workspace at runtime
 *   3. Rely on client-side routing for actual workspace-scoped paths
 *
 * In production (static export), deep-linking to a specific workspace settings page
 * will 404 unless the hub serves a fallback (the Go binary serves index.html for all
 * unmatched routes, allowing the SPA to handle the path client-side).
 */
export function generateStaticParams() {
  // LEGACY_ANALYTICS_SECTION is pre-rendered as a redirect-only stub: the hub
  // maps hard loads of /settings/{ws}/workspace-analytics to these files
  // (settingsStaticSection in pkg/hub/server.go), and without them the SPA
  // fallback would serve the root shell and the redirect to /analytics in
  // settings-content.tsx could never run for bookmarks and shared links.
  return [
    { parts: [] },
    ...VALID_SECTIONS.map((section) => ({ parts: [section] })),
    { parts: [LEGACY_ANALYTICS_SECTION] },
    { parts: ["_workspace"] },
    ...VALID_SECTIONS.map((section) => ({ parts: ["_workspace", section] })),
    { parts: ["_workspace", LEGACY_ANALYTICS_SECTION] },
  ]
}

export default function Page() {
  // SettingsSectionPage reads usePathname/useParams, which can suspend under
  // static export; without this boundary the whole route would suspend.
  return (
    <Suspense fallback={null}>
      <SettingsSectionPage />
    </Suspense>
  )
}
