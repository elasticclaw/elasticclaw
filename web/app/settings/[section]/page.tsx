import SettingsSectionPage from "./settings-content"

const VALID_SECTIONS = ["runtimes", "models", "github", "authentication", "issue-trackers", "factories", "secrets", "templates", "ai-config"]

export function generateStaticParams() {
  return VALID_SECTIONS.map((section) => ({ section }))
}

export default function Page() {
  return <SettingsSectionPage />
}
