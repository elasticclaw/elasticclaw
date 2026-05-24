import SettingsSectionPage from "./settings-content"
import { VALID_SECTIONS } from "./sections"

export function generateStaticParams() {
  return [
    { parts: [] },
    ...VALID_SECTIONS.map((section) => ({ parts: [section] })),
    { parts: ["_workspace"] },
    ...VALID_SECTIONS.map((section) => ({ parts: ["_workspace", section] })),
  ]
}

export default function Page() {
  return <SettingsSectionPage />
}
