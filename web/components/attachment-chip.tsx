"use client"

import { useState } from "react"
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
//
// Visually: pre-submit chips are fixed-size thumbs/pills inside the composer
// strip; history images fill a 4:3 frame in the message's attachment grid and
// history files are plain rows inside the bubble.
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
  const interactive = !!onRemove

  const imgSrc =
    source?.kind === "preview"
      ? source.url
      : source?.kind === "history"
        ? getFileViewUrl(source.clawId, source.path)
        : undefined

  const removeButton = onRemove && (
    <button
      type="button"
      onClick={(e) => { e.stopPropagation(); e.preventDefault(); onRemove() }}
      className={cn(
        "flex items-center justify-center rounded-full text-muted-foreground transition-colors hover:text-foreground",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring",
        // 44px touch target on mobile without growing the 20px visual.
        "max-md:before:absolute max-md:before:-inset-3 max-md:before:content-['']",
        isImage ? "absolute right-1 top-1 size-5 bg-background/80 backdrop-blur-sm" : "relative size-5 shrink-0 hover:bg-accent/40"
      )}
      aria-label={`Remove ${name}`}
    >
      <X className="size-3" />
    </button>
  )

  if (isImage && imgSrc) {
    const Wrap = source?.kind === "history" ? "a" : "div"
    const frame = cn(
      "relative block overflow-hidden rounded-lg border bg-background/70",
      status === "error" ? "border-destructive/50 opacity-60" : "border-border/80",
      interactive ? (size === "sm" ? "h-12 w-12" : "h-16 w-16") : "aspect-[4/3] w-full"
    )
    const wrapProps = source?.kind === "history"
      ? { href: imgSrc, target: "_blank", rel: "noreferrer", className: frame }
      : { className: frame }
    return (
      <Wrap {...wrapProps as object} title={`${name} (${sizeLabel})`}>
        <img
          src={imgSrc}
          alt={name}
          onError={() => setBroken(true)}
          loading="lazy"
          className={cn("block size-full", source?.kind === "history" ? "object-contain" : "object-cover")}
        />
        {status === "uploading" && (
          <div className="absolute inset-0 flex items-center justify-center bg-background/60">
            <Loader2 className={size === "sm" ? "size-3 animate-spin" : "size-4 animate-spin"} />
          </div>
        )}
        {removeButton}
      </Wrap>
    )
  }

  // Text chip (non-image, broken image, or history without source).
  const StatusIcon =
    status === "uploading" ? Loader2 : status === "error" ? AlertCircle : FileIcon
  const iconCls = cn("shrink-0", size === "sm" ? "size-3" : "size-3.5", status === "uploading" && "animate-spin")

  return (
    <div
      className={cn(
        "flex min-w-0 items-center gap-2",
        size === "sm" ? "text-xs" : "text-sm",
        interactive && "rounded-lg border px-2.5 py-1",
        status === "error"
          ? "border-destructive/50 bg-error-surface text-error-foreground"
          : interactive
            ? "border-border/80 bg-background/70 text-foreground"
            : "py-1 text-foreground/80"
      )}
      title={error || path || `${name}${mimetype ? ` (${mimetype})` : ""}`}
    >
      <StatusIcon className={iconCls} />
      <span className={cn("truncate", size === "sm" ? "max-w-[7rem]" : "max-w-[14rem]")}>{name}</span>
      <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{sizeLabel}</span>
      {removeButton}
    </div>
  )
}
