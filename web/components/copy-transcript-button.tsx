"use client"

import { useCallback, useState } from "react"
import { Check, ClipboardCopy } from "lucide-react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
import type { Message } from "@/lib/types"
import {
  copyTextToClipboard,
  formatChatTranscript,
  type TranscriptClawMeta,
} from "@/lib/transcript"

type Size = "icon" | "sm"

interface CopyTranscriptButtonProps {
  claw: TranscriptClawMeta
  messages: Message[]
  streamingText?: string
  size?: Size
  className?: string
  /** When true, stop click from bubbling (board cards). */
  stopPropagation?: boolean
}

export function CopyTranscriptButton({
  claw,
  messages,
  streamingText,
  size = "sm",
  className,
  stopPropagation = false,
}: CopyTranscriptButtonProps) {
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)

  const handleCopy = useCallback(
    async (e: React.MouseEvent) => {
      if (stopPropagation) {
        e.stopPropagation()
        e.preventDefault()
      }
      if (busy) return
      setBusy(true)
      const text = formatChatTranscript({
        claw,
        messages,
        streamingText,
      })
      const ok = await copyTextToClipboard(text)
      setBusy(false)
      if (!ok) return
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    },
    [busy, claw, messages, streamingText, stopPropagation]
  )

  const disabled = busy || (messages.length === 0 && !streamingText?.trim())
  const title = copied
    ? "Copied"
    : disabled
      ? "No messages to copy"
      : "Copy chat transcript"

  if (size === "icon") {
    return (
      <button
        type="button"
        onClick={handleCopy}
        disabled={disabled}
        title={title}
        className={cn(
          "p-1 rounded hover:bg-accent transition-colors disabled:opacity-40 disabled:pointer-events-none",
          className
        )}
      >
        {copied ? (
          <Check className="size-3.5 text-green-500" />
        ) : (
          <ClipboardCopy className="size-3.5 text-muted-foreground" />
        )}
      </button>
    )
  }

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={handleCopy}
      disabled={disabled}
      title={title}
      className={className}
    >
      {copied ? (
        <Check className="size-3.5 mr-1.5 text-green-500" />
      ) : (
        <ClipboardCopy className="size-3.5 mr-1.5" />
      )}
      {copied ? "Copied" : "Copy transcript"}
    </Button>
  )
}
