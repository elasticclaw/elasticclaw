"use client"

import { isValidElement, memo, useCallback, useEffect, useState, useSyncExternalStore, type ReactNode } from "react"
import ReactMarkdown, { type Components } from "react-markdown"
import remarkGfm from "remark-gfm"
import { Check, Copy, WrapText } from "lucide-react"
import { cn } from "@/lib/utils"
import { copyTextToClipboard } from "@/lib/transcript"

interface MarkdownContentProps {
  content: string
  className?: string
  /** Marks the root so newly inserted blocks fade in (see .chat-markdown[data-streaming]). */
  streaming?: boolean
}

// ─── shiki ────────────────────────────────────────────────────────────────────
// One lazily imported bundle per page; shiki's shorthand keeps its own singleton
// highlighter and loads grammars on demand, so the first paint never waits on
// it. Output is cached per (theme, lang, code) with a small LRU so a streaming
// block re-highlights only for text that actually changed.

type Shiki = typeof import("shiki")
let shikiModule: Promise<Shiki> | null = null
const loadShiki = () => (shikiModule ??= import("shiki"))

const HIGHLIGHT_CACHE_MAX = 200
const highlightCache = new Map<string, string>()

type ThemeName = "github-dark" | "github-light"

function themeName(): ThemeName {
  if (typeof document === "undefined") return "github-dark"
  return document.documentElement.classList.contains("dark") ? "github-dark" : "github-light"
}

// The app toggles `.dark` on <html>; watch it so already-mounted blocks
// re-highlight with the matching theme instead of keeping the old palette.
function subscribeTheme(onChange: () => void) {
  if (typeof document === "undefined") return () => {}
  const observer = new MutationObserver(onChange)
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] })
  return () => observer.disconnect()
}

function useThemeName(): ThemeName {
  return useSyncExternalStore(subscribeTheme, themeName, () => "github-dark")
}

async function highlight(code: string, lang: string, theme: string): Promise<string | null> {
  const shiki = await loadShiki()
  if (!(lang in shiki.bundledLanguages)) return null
  const html = await shiki.codeToHtml(code, { lang, theme })
  return html
}

function useHighlightedHtml(code: string, lang: string | undefined): string | null {
  const [entry, setEntry] = useState<{ key: string; html: string } | null>(null)
  const theme = useThemeName()
  const key = lang ? `${theme}\0${lang}\0${code}` : ""
  // Read the cache during render: a warm block paints highlighted on its first
  // frame instead of flashing plain for one effect tick.
  const cached = key ? highlightCache.get(key) : undefined

  useEffect(() => {
    if (!key || highlightCache.has(key)) return
    let cancelled = false
    highlight(code, lang!, theme)
      .then((html) => {
        if (cancelled || !html) return
        if (highlightCache.size >= HIGHLIGHT_CACHE_MAX) {
          highlightCache.delete(highlightCache.keys().next().value as string)
        }
        highlightCache.set(key, html)
        setEntry({ key, html })
      })
      .catch(() => {})
    return () => { cancelled = true }
  }, [key, code, lang, theme])

  if (cached) return cached
  return entry?.key === key ? entry.html : null
}

// ─── code block ───────────────────────────────────────────────────────────────

const CodeBlock = memo(function CodeBlock({ code, lang, streaming }: { code: string; lang?: string; streaming?: boolean }) {
  const [wrapped, setWrapped] = useState(false)
  const [copied, setCopied] = useState(false)
  // Highlighting every 125ms sample of a growing block is wasted work; paint
  // plain while streaming and highlight once the message finalizes.
  const html = useHighlightedHtml(code, streaming ? undefined : lang)

  const copy = useCallback(async () => {
    if (!(await copyTextToClipboard(code))) return
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1200)
  }, [code])

  const action = "flex size-6 items-center justify-center rounded-sm text-foreground/72 transition-colors hover:text-foreground aria-pressed:bg-foreground/8 aria-pressed:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring max-md:size-11"

  return (
    <div
      className="chat-markdown-codeblock my-[0.65rem] overflow-hidden rounded-[var(--radius)] border border-border/70 bg-secondary leading-snug dark:border-transparent dark:bg-input/32"
      data-language={lang}
      data-wrap={wrapped ? "true" : "false"}
    >
      <div className="flex select-none items-center justify-between gap-2 pt-1.5 pr-1.5 pb-0 pl-3">
        <span className="min-w-0 truncate font-mono text-[0.6875rem] text-foreground/72">{lang ?? "text"}</span>
        <span className="flex items-center gap-0.5">
          <button
            type="button"
            className={action}
            aria-pressed={wrapped}
            aria-label="Wrap lines"
            title="Wrap lines"
            onClick={() => setWrapped((value) => !value)}
          >
            <WrapText className="size-3" />
          </button>
          <button
            type="button"
            className={action}
            aria-label="Copy code"
            title={copied ? "Copied" : "Copy code"}
            onClick={copy}
          >
            {copied ? <Check className="size-3" /> : <Copy className="size-3" />}
            <span role="status" className="sr-only">{copied ? "Copied" : ""}</span>
          </button>
        </span>
      </div>
      {html ? (
        <div dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        <pre><code>{code}</code></pre>
      )}
    </div>
  )
})

function nodeText(node: ReactNode): string {
  if (node == null || typeof node === "boolean") return ""
  if (typeof node === "string" || typeof node === "number") return String(node)
  if (Array.isArray(node)) return node.map(nodeText).join("")
  if (isValidElement<{ children?: ReactNode }>(node)) return nodeText(node.props.children)
  return ""
}

// ─── react-markdown wiring ────────────────────────────────────────────────────
// Element styling lives in the .chat-markdown CSS block (globals.css); the
// components here only add the pieces CSS cannot: the code block frame, the
// table scroll container and external link targets.

const remarkPlugins = [remarkGfm]

const buildComponents = (streaming: boolean): Components => ({
  pre: ({ children }) => {
    const child = isValidElement<{ className?: string; children?: ReactNode }>(children) ? children : null
    const lang = /language-([\w+#.-]+)/.exec(child?.props.className ?? "")?.[1]?.toLowerCase()
    const code = nodeText(child?.props.children ?? children).replace(/\n$/, "")
    return <CodeBlock code={code} lang={lang} streaming={streaming} />
  },
  table: ({ children }) => (
    <div className="chat-markdown-table-container">
      <table>{children}</table>
    </div>
  ),
  a: ({ href, children }) => (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  ),
})

const staticComponents = buildComponents(false)
const streamingComponents = buildComponents(true)

export const MarkdownContent = memo(function MarkdownContent({ content, className, streaming }: MarkdownContentProps) {
  return (
    <div className={cn("chat-markdown", className)} data-streaming={streaming ? "" : undefined}>
      <ReactMarkdown remarkPlugins={remarkPlugins} components={streaming ? streamingComponents : staticComponents}>
        {content}
      </ReactMarkdown>
    </div>
  )
})
