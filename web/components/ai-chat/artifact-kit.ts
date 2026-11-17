import type { AIChatArtifactCard, AIChatArtifactKit, AIChatDesignSystemStatus, AIChatTemplate } from "@/lib/api"
import { artifactURLExpired } from "@/lib/ai-chat-artifacts"
import { sourceStatus } from "./sources"

export function kitTemplates(kit?: AIChatArtifactKit): AIChatTemplate[] {
  return kit?.templates ?? []
}

export function effectivePin(pin: string, kit?: AIChatArtifactKit) {
  return pin === "free" || kitTemplates(kit).some((template) => template.id === pin) ? pin : ""
}

export function pinLabel(pin: string, kit?: AIChatArtifactKit) {
  return pin === "free" ? "Free" : kitTemplates(kit).find((template) => template.id === pin)?.name ?? "Auto"
}

export function templateLabel(card: AIChatArtifactCard, kit?: AIChatArtifactKit) {
  return card.templateName ?? card.template ?? (kitTemplates(kit).length ? "No template" : "")
}

export function kitWarnings(kit?: AIChatArtifactKit): string[] {
  return [
    ...(kit?.designSystem.status === "invalid" ? [`Design system: ${kit.designSystem.error || "Invalid configuration."} Artifacts are built without it.`] : []),
    ...(kit?.warnings ?? []),
  ]
}

export function designSystemLabel(status: AIChatDesignSystemStatus) {
  const source = sourceStatus(status === "ok" ? "connected" : status === "invalid" ? "invalid" : "unknown")
  return { ...source, label: status === "ok" ? "Applied" : status === "invalid" ? "Invalid" : "Not set" }
}

export function templatePreviewExpired(url: string) {
  return artifactURLExpired(url)
}
