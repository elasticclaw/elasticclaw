import type { Message } from "@/lib/types"

export type MessageAuthor =
  | { kind: "self" }
  | { kind: "teammate"; login: string; name: string; initials: string; color: string }
  | { kind: "agent" }

const PERSON_COLORS = ["var(--chart-1)", "var(--chart-2)", "var(--chart-3)", "var(--chart-4)", "var(--chart-5)"]

export function personColor(login: string): string {
  let hash = 0
  for (let index = 0; index < login.length; index += 1) {
    hash = (hash * 31 + login.charCodeAt(index)) >>> 0
  }
  return PERSON_COLORS[hash % PERSON_COLORS.length]
}

function initials(login: string): string {
  const parts = login.split(/[._\-\s]+/).filter(Boolean)
  return (parts.length > 1 ? parts.map((part) => part[0]).join("") : login.slice(0, 2)).toUpperCase()
}

export function messageAuthor(message: Message, me?: string | null): MessageAuthor {
  if (message.role !== "user") return { kind: "agent" }
  if (!message.userLogin || message.userLogin === me) return { kind: "self" }
  return { login: message.userLogin, name: message.userLogin, initials: initials(message.userLogin), color: personColor(message.userLogin), kind: "teammate" }
}
