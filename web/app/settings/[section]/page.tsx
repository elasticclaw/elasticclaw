import SettingsSectionPage from "./settings-content"

const VALID_SECTIONS = ["runtimes", "llm", "github", "security", "integrations", "factories", "secrets", "templates"]

export function generateStaticParams() {
  return VALID_SECTIONS.map((section) => ({ section }))
}

export default function Page() {
  return <SettingsSectionPage />
}
