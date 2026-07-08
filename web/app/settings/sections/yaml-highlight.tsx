"use client"

import { useEffect, useState } from "react"

export function YamlHighlight({ code }: { code: string }) {
  const [html, setHtml] = useState<string | null>(null)
  useEffect(() => {
    if (!code) return
    import("shiki").then(({ codeToHtml }) =>
      codeToHtml(code, { lang: "yaml", theme: "github-dark" })
    ).then(setHtml).catch(() => setHtml(null))
  }, [code])
  if (!html) return <pre className="h-full overflow-auto p-3 text-xs font-mono leading-relaxed whitespace-pre">{code}</pre>
  return <div className="h-full overflow-auto p-3 text-xs leading-relaxed [&_pre]:!bg-transparent [&_code]:!text-xs [&_code]:!font-mono" dangerouslySetInnerHTML={{ __html: html }} />
}
