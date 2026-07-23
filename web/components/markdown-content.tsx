"use client"

import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { cn } from "@/lib/utils"

interface MarkdownContentProps {
  content: string
  className?: string
}

export function MarkdownContent({ content, className }: MarkdownContentProps) {
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
              <code className="block bg-black/40 rounded px-3 py-2 text-xs font-mono whitespace-pre overflow-x-auto my-2">
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
      {content}
    </ReactMarkdown>
  )
}
