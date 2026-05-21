import SettingsSectionPage from "./settings-content"

const VALID_SECTIONS = ["runtimes", "models", "github", "authentication", "issue-trackers", "factories", "secrets", "mcp-servers", "templates", "ai-config", "webhooks", "doctor", "analytics", "troubleshoot"]

export function generateStaticParams() {
  return VALID_SECTIONS.map((section) => ({ section }))
}

export default function Page() {
  return <SettingsSectionPage />
}
