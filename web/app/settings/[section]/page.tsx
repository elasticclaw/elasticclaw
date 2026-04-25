import SettingsSectionPage from "./settings-content"

const VALID_SECTIONS = ["runtimes", "llm", "github", "authentication", "integrations", "factories", "secrets", "templates"]

export function generateStaticParams() {
  return VALID_SECTIONS.map((section) => ({ section }))
}

export default function Page() {
  return <SettingsSectionPage />
}
