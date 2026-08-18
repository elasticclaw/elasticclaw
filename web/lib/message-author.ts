import { personColor } from "@/components/ds"
import type { Message } from "@/lib/types"

export type MessageAuthor =
  | { kind: "self" }
  | { kind: "teammate"; login: string; name: string; initials: string; color: string }
  | { kind: "unknown" }
  | { kind: "agent" }

function initials(login: string): string {
  const parts = login.split(/[._\-\s]+/).filter(Boolean)
  return (parts.length > 1 ? parts.map((part) => part[0]).join("") : login.slice(0, 2)).toUpperCase()
}

export function messageAuthor(message: Message, me?: string | null): MessageAuthor {
  if (message.role !== "user") return { kind: "agent" }
  if (message.optimisticSelf || (message.userLogin && me && message.userLogin === me)) return { kind: "self" }
  if (!message.userLogin) return { kind: "unknown" }
  return { login: message.userLogin, name: message.userLogin, initials: initials(message.userLogin), color: personColor(message.userLogin), kind: "teammate" }
}
