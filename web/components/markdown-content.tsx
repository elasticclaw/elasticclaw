"use client"

import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { cn } from "@/lib/utils"

interface MarkdownContentProps {
  content: string
  className?: string
}

/**
 * Normalize agent output so ReactMarkdown can parse it correctly.
 * Agents often produce numbered lists without blank lines between items,
 * or jam multiple list items onto one line separated by patterns like "1. ... 2. ..."
 */
function normalizeMarkdown(content: string): string {
  return content
    // Insert newline before numbered list items that follow non-newline content
    // e.g. "done**2. Next step" → "done\n\n**2. Next step"
    .replace(/([^\n])(\*{0,2}\d+\.\s)/g, (_, before, item) => `${before}\n\n${item}`)
    // Insert newline before bullet points jammed after content
    .replace(/([^\n])(\*{0,2}[-*]\s)/g, (_, before, item) => `${before}\n\n${item}`)
    // Ensure \n\n before headers if not already there
    .replace(/([^\n])(#{1,3}\s)/g, (_, before, header) => `${before}\n\n${header}`)
    // Collapse 3+ consecutive newlines to 2
    .replace(/\n{3,}/g, '\n\n')
    .trim()
}

export function MarkdownContent({ content, className }: MarkdownContentProps) {
  const normalized = normalizeMarkdown(content)
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      className={cn("prose prose-sm prose-invert max-w-none", className)}
      components={{
        // Headings
        h1: ({ children }) => <h1 className="text-base font-bold mt-3 mb-1 first:mt-0">{children}</h1>,
        h2: ({ children }) => <h2 className="text-sm font-bold mt-3 mb-1 first:mt-0">{children}</h2>,
        h3: ({ children }) => <h3 className="text-sm font-semibold mt-2 mb-1 first:mt-0">{children}</h3>,

        // Paragraphs — no margin on first/last to avoid extra whitespace in bubbles
        p: ({ children }) => <p className="mb-2 last:mb-0 leading-relaxed">{children}</p>,

        // Code
        code: ({ children, className }) => {
          const isBlock = className?.startsWith("language-")
          if (isBlock) {
            return (
              <code className="block bg-black/40 rounded px-3 py-2 text-xs font-mono whitespace-pre my-2">
                {children}
              </code>
            )
          }
          return (
            <code className="bg-black/40 rounded px-1.5 py-0.5 text-xs font-mono break-words">{children}</code>
          )
        },
        pre: ({ children }) => <pre className="not-prose my-2 overflow-x-auto">{children}</pre>,

        // Lists
        ul: ({ children }) => <ul className="list-disc list-outside ml-4 mb-2 last:mb-0 space-y-0.5">{children}</ul>,
        ol: ({ children }) => <ol className="list-decimal list-outside ml-4 mb-2 last:mb-0 space-y-0.5">{children}</ol>,
        li: ({ children }) => <li className="text-sm leading-relaxed">{children}</li>,

        // Blockquote
        blockquote: ({ children }) => (
          <blockquote className="border-l-2 border-muted-foreground/40 pl-3 my-2 text-muted-foreground italic">
            {children}
          </blockquote>
        ),

        // Tables (GFM)
        table: ({ children }) => (
          <div className="overflow-x-auto my-2">
            <table className="text-xs border-collapse w-full">{children}</table>
          </div>
        ),
        th: ({ children }) => (
          <th className="border border-border px-2 py-1 bg-muted text-left font-semibold">{children}</th>
        ),
        td: ({ children }) => (
          <td className="border border-border px-2 py-1">{children}</td>
        ),

        // Links
        a: ({ href, children }) => (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="text-blue-400 hover:underline"
          >
            {children}
          </a>
        ),

        // HR
        hr: () => <hr className="border-border my-3" />,

        // Strong / em
        strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
        em: ({ children }) => <em className="italic">{children}</em>,
      }}
    >
      {normalized}
    </ReactMarkdown>
  )
}
