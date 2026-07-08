"use client"

import React from "react"

export interface ChatMessage {
  role: "user" | "assistant"
  content: string
  streaming?: boolean
}

export function normalizeStoredMessage(message: ChatMessage): ChatMessage {
  return {
    ...message,
    content: message.content.replace(/▌$/, ""),
    streaming: false,
  }
}

// Simple markdown renderer for assistant messages (no external deps)
// Strip ```yaml ... ``` blocks and any open/incomplete yaml code block at the end.
// Applied during streaming so YAML never appears in chat.
export function stripYamlBlocks(text: string): string {
  // Remove complete yaml blocks
  let result = text.replace(/```ya?ml[\s\S]*?```/gi, "")
  // Remove any block that looks like a hub.yaml (complete)
  result = result.replace(/```[\s\S]*?```/g, (m) => {
    if (m.includes("claw_token:") || m.includes("url: http")) return ""
    return m
  })
  // Remove incomplete/open yaml block at the end (streaming: block started but not closed)
  result = result.replace(/```ya?ml[\s\S]*$/i, "")
  // Also remove any open ``` block at the end if it looks like hub.yaml
  result = result.replace(/```[\s\S]*$/g, (m) => {
    if (m.includes("claw_token:") || m.includes("url:") || m.includes("token:")) return ""
    return m
  })
  return result.trim()
}

export function renderMarkdown(text: string): React.ReactNode[] {
  // Split off fenced code blocks first
  const parts = text.split(/(```[\s\S]*?```)/g)
  const nodes: React.ReactNode[] = []
  let globalKey = 0
  parts.forEach((part, pi) => {
    if (part.startsWith("```") && part.endsWith("```")) {
      const inner = part.slice(3, -3).replace(/^[^\n]*\n/, "") // strip language hint
      nodes.push(
        <pre key={`cb-${pi}`} className="bg-muted rounded p-2 my-1 overflow-x-auto">
          <code className="text-xs font-mono">{inner}</code>
        </pre>
      )
      return
    }
    // Process line by line
    const lines = part.split("\n")
    const lineNodes: React.ReactNode[] = []
    let ulItems: React.ReactNode[] = []
    let ulKey = 0
    const flushList = () => {
      if (ulItems.length > 0) {
        lineNodes.push(<ul key={`ul-${pi}-${ulKey++}`} className="list-disc pl-4 my-1 space-y-0.5">{ulItems}</ul>)
        ulItems = []
      }
    }
    lines.forEach((line, li) => {
      const isList = /^[\-\*]\s+/.test(line)
      if (!isList) flushList()
      if (isList) {
        ulItems.push(<li key={`li-${pi}-${li}`}>{inlineMarkdown(line.replace(/^[\-\*]\s+/, ""))}</li>)
      } else if (line.trim() === "") {
        lineNodes.push(<br key={`br-${pi}-${li}`} />)
      } else {
        lineNodes.push(<span key={`s-${pi}-${li}`}>{inlineMarkdown(line)}<br /></span>)
      }
    })
    flushList()
    nodes.push(...lineNodes.map((n, i) => React.cloneElement(n as React.ReactElement, { key: `n-${globalKey++}-${i}` })))
  })
  return nodes
}

function inlineMarkdown(text: string): React.ReactNode[] {
  // Split on inline code, bold, italic
  const tokens = text.split(/(``[^`]+``|`[^`]+`|\*\*[^*]+\*\*|\*[^*]+\*)/g)
  return tokens.map((tok, i) => {
    if (tok.startsWith("**") && tok.endsWith("**")) return <strong key={i}>{tok.slice(2, -2)}</strong>
    if (tok.startsWith("*") && tok.endsWith("*")) return <em key={i}>{tok.slice(1, -1)}</em>
    if (tok.startsWith("`") && tok.endsWith("`")) return <code key={i} className="bg-muted px-1 rounded text-xs font-mono">{tok.slice(1, -1)}</code>
    return tok
  })
}

export function extractYamlPlaceholders(yaml: string): string[] {
  const seen = new Set<string>()
  const placeholders: string[] = []
  for (const match of yaml.matchAll(/__([A-Z0-9_]+)__/g)) {
    const name = match[1]
    if (name && !seen.has(name)) {
      seen.add(name)
      placeholders.push(name)
    }
  }
  return placeholders
}
