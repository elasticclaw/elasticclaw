import SettingsSectionPage from "./settings-content"
import { VALID_SECTIONS } from "./sections"

export function generateStaticParams() {
  return VALID_SECTIONS.map((section) => ({ section }))
}

export default function Page() {
  return <SettingsSectionPage />
}
