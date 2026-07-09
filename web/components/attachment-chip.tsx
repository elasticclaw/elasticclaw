"use client"

import { useEffect, useState } from "react"
import { AlertCircle, File as FileIcon, Loader2, X } from "lucide-react"
import { cn } from "@/lib/utils"
import { getFileViewUrl } from "@/lib/api"

// Source is either a Blob URL we already hold (pre-submit) or a (clawId, path)
// pair the hub serves back from the claw (history). History renders fall back
// to a text chip on <img> load error.
export type AttachmentChipSource =
  | { kind: "preview"; url: string }
  | { kind: "history"; clawId: string; path: string }

export type AttachmentChipStatus = "uploading" | "ready" | "error"

interface Props {
  name: string
  sizeLabel: string
  mimetype: string
  source?: AttachmentChipSource
  size?: "sm" | "md"
  status?: AttachmentChipStatus
  error?: string
  path?: string
  onRemove?: () => void
}

// AttachmentChip is the single chip render for the attachments feature.
// Handles three modes: pre-submit (status set, remove button), post-submit
// history (no status, may have onError fallback), and thumbnail-vs-text variants.
export function AttachmentChip({
  name,
  sizeLabel,
  mimetype,
  source,
  size = "md",
  status,
  error,
  path,
  onRemove,
}: Props) {
  const [broken, setBroken] = useState(false)
  const isImage = mimetype.startsWith("image/") && !broken && !!source

  // History files are served by the hub behind a single-use auth ticket, so
  // the <img> URL must be resolved asynchronously (one fresh ticket per URL).
  const historyClawId = source?.kind === "history" ? source.clawId : undefined
  const historyPath = source?.kind === "history" ? source.path : undefined
  const [historySrc, setHistorySrc] = useState<string | undefined>(undefined)
  useEffect(() => {
    if (!historyClawId || !historyPath) return
    let cancelled = false
    getFileViewUrl(historyClawId, historyPath)
      .then((url) => { if (!cancelled) setHistorySrc(url) })
      .catch(() => { if (!cancelled) setBroken(true) })
    return () => { cancelled = true }
  }, [historyClawId, historyPath])

  const imgSrc = source?.kind === "preview" ? source.url : historySrc

  // Opening the full image in a new tab needs its own fresh ticket — the
  // ticket in imgSrc was already redeemed by the <img> load.
  const openHistoryFile = async (e: React.MouseEvent) => {
    e.preventDefault()
    if (!historyClawId || !historyPath) return
    try {
      const url = await getFileViewUrl(historyClawId, historyPath)
      window.open(url, "_blank", "noreferrer")
    } catch {
      /* hub unreachable — keep the chip as-is */
    }
  }

  const thumbCls = size === "sm"
    ? "max-h-16 max-w-[6rem]"
    : "max-h-24 max-w-[8rem]"

  if (isImage && imgSrc) {
    const Wrap = source?.kind === "history" ? "a" : "div"
    const wrapProps = source?.kind === "history"
      ? { href: "#", onClick: openHistoryFile, className: "relative block" }
      : { className: "relative" }
    return (
      <Wrap {...wrapProps as object} title={`${name} (${sizeLabel})`}>
        <img
          src={imgSrc}
          alt={name}
          onError={() => setBroken(true)}
          loading="lazy"
          className={cn(
            thumbCls,
            "rounded-md border",
            source?.kind === "history" ? "object-contain bg-background/40" : "object-cover",
            status === "error" ? "border-destructive/50 opacity-60" : "border-border"
          )}
        />
        {status === "uploading" && (
          <div className="absolute inset-0 flex items-center justify-center rounded-md bg-background/60">
            <Loader2 className={size === "sm" ? "size-3 animate-spin" : "size-4 animate-spin"} />
          </div>
        )}
        {onRemove && (
          <button
            type="button"
            onClick={(e) => { e.stopPropagation(); e.preventDefault(); onRemove() }}
            className={cn(
              "absolute rounded-full bg-background border border-border text-muted-foreground hover:text-foreground shadow-sm",
              size === "sm" ? "-top-1 -right-1 p-0.5" : "-top-1.5 -right-1.5 p-0.5"
            )}
            aria-label={`Remove ${name}`}
          >
            <X className={size === "sm" ? "size-2.5" : "size-3"} />
          </button>
        )}
      </Wrap>
    )
  }

  // Text chip (non-image, broken image, or history without source).
  const iconSize = size === "sm" ? "size-2.5" : "size-3"
  const textSize = size === "sm" ? "text-[10px]" : "text-xs"
  const pad = size === "sm" ? "px-1.5 py-0.5" : "px-2 py-0.5"
  const nameMax = size === "sm" ? "max-w-[7rem]" : "max-w-[14rem]"

  const StatusIcon =
    status === "uploading" ? Loader2 : status === "error" ? AlertCircle : FileIcon
  const statusIconCls = status === "uploading" ? `${iconSize} animate-spin` : iconSize

  const interactive = !!onRemove
  return (
    <div
      className={cn(
        "flex items-center gap-1.5 rounded-md border",
        pad,
        textSize,
        status === "error"
          ? "border-destructive/50 bg-destructive/10 text-destructive"
          : interactive
            ? "border-border bg-secondary text-foreground"
            : "border-border/60 bg-background/40 text-muted-foreground"
      )}
      title={error || path || `${name}${mimetype ? ` (${mimetype})` : ""}`}
    >
      <StatusIcon className={statusIconCls} />
      <span className={cn("truncate", nameMax)}>{name}</span>
      <span className={interactive ? "text-muted-foreground" : undefined}>{sizeLabel}</span>
      {onRemove && (
        <button
          type="button"
          onClick={(e) => { e.stopPropagation(); onRemove() }}
          className="ml-0.5 text-muted-foreground hover:text-foreground"
          aria-label={`Remove ${name}`}
        >
          <X className={iconSize} />
        </button>
      )}
    </div>
  )
}
