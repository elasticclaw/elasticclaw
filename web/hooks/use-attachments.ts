"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { uploadFiles } from "@/lib/api"
import {
  MAX_FILE_BYTES,
  MAX_FILES_PER_MSG,
  type PendingAttachment,
} from "@/lib/attachments"

export interface UseAttachmentsApi {
  attachments: PendingAttachment[]
  dragHover: boolean
  addFiles: (files: File[]) => void
  removeAttachment: (localId: string) => void
  clearAttachments: () => void
  onDragEnter: (e: React.DragEvent) => void
  onDragOver: (e: React.DragEvent) => void
  onDragLeave: (e: React.DragEvent) => void
  onDrop: (e: React.DragEvent) => void
  onPaste: (e: React.ClipboardEvent) => void
}

// useAttachments owns the per-view attachment state used by both the full chat
// view and the per-claw board card. Each consumer gets its own instance keyed
// to a claw; object URLs for image previews are revoked on remove, clear, and
// unmount so no memory leaks when a card or chat view is torn down mid-upload.
export function useAttachments(clawId: string): UseAttachmentsApi {
  const [attachments, setAttachments] = useState<PendingAttachment[]>([])
  const [dragHover, setDragHover] = useState(false)

  // attachmentsRef is the single source of truth for count/membership inside
  // this hook. It is updated synchronously alongside every setAttachments call
  // so that rapid successive calls (drop + paste in the same tick) see the
  // correct pending count, rather than a stale closure/state.
  const attachmentsRef = useRef<PendingAttachment[]>([])
  // Sync on render too, so async upload callbacks that mutate via functional
  // updaters don't let the ref drift from committed state.
  useEffect(() => {
    attachmentsRef.current = attachments
  }, [attachments])
  useEffect(() => {
    return () => {
      for (const a of attachmentsRef.current) {
        if (a.previewUrl) URL.revokeObjectURL(a.previewUrl)
      }
    }
  }, [])

  const addFiles = useCallback((picked: File[]) => {
    if (picked.length === 0) return
    const accepted: File[] = []
    for (const f of picked) {
      if (f.size > MAX_FILE_BYTES) {
        alert(`${f.name} is larger than 20 MB — skipped.`)
        continue
      }
      accepted.push(f)
    }
    if (accepted.length === 0) return
    if (attachmentsRef.current.length + accepted.length > MAX_FILES_PER_MSG) {
      alert(`At most ${MAX_FILES_PER_MSG} files per message.`)
      return
    }
    const entries: PendingAttachment[] = accepted.map((f) => {
      const mimetype = f.type || "application/octet-stream"
      return {
        localId: `pa-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
        name: f.name,
        size: f.size,
        mimetype,
        previewUrl: mimetype.startsWith("image/") ? URL.createObjectURL(f) : undefined,
        status: "uploading",
      }
    })
    // Advance the ref synchronously before scheduling state so the next
    // synchronous addFiles call sees the updated count.
    attachmentsRef.current = [...attachmentsRef.current, ...entries]
    setAttachments(attachmentsRef.current)

    uploadFiles(clawId, accepted)
      .then((uploaded) => {
        setAttachments((prev) => {
          const next = [...prev]
          entries.forEach((e, i) => {
            const idx = next.findIndex((p) => p.localId === e.localId)
            if (idx >= 0 && uploaded[i]) {
              next[idx] = { ...next[idx], status: "ready", path: uploaded[i].path }
            }
          })
          attachmentsRef.current = next
          return next
        })
      })
      .catch((err) => {
        console.error("upload failed", err)
        setAttachments((prev) => {
          const next = prev.map((p) =>
            entries.some((e) => e.localId === p.localId)
              ? { ...p, status: "error" as const, error: String(err) }
              : p
          )
          attachmentsRef.current = next
          return next
        })
      })
  }, [clawId])

  const removeAttachment = useCallback((localId: string) => {
    setAttachments((prev) => {
      const gone = prev.find((p) => p.localId === localId)
      if (gone?.previewUrl) URL.revokeObjectURL(gone.previewUrl)
      const next = prev.filter((p) => p.localId !== localId)
      attachmentsRef.current = next
      return next
    })
  }, [])

  const clearAttachments = useCallback(() => {
    setAttachments((prev) => {
      for (const a of prev) {
        if (a.previewUrl) URL.revokeObjectURL(a.previewUrl)
      }
      attachmentsRef.current = []
      return []
    })
  }, [])

  const dragCounterRef = useRef(0)

  const onDragEnter = useCallback((e: React.DragEvent) => {
    if (e.dataTransfer.types.includes("Files")) {
      e.preventDefault()
      dragCounterRef.current++
      setDragHover(true)
    }
  }, [])
  const onDragOver = useCallback((e: React.DragEvent) => {
    if (e.dataTransfer.types.includes("Files")) {
      e.preventDefault()
    }
  }, [])
  const onDragLeave = useCallback((e: React.DragEvent) => {
    dragCounterRef.current--
    if (dragCounterRef.current <= 0) {
      dragCounterRef.current = 0
      setDragHover(false)
    }
  }, [])
  const onDrop = useCallback((e: React.DragEvent) => {
    if (!e.dataTransfer.files || e.dataTransfer.files.length === 0) return
    e.preventDefault()
    dragCounterRef.current = 0
    setDragHover(false)
    addFiles(Array.from(e.dataTransfer.files))
  }, [addFiles])
  const onPaste = useCallback((e: React.ClipboardEvent) => {
    const items = e.clipboardData?.items
    if (!items) return
    const files: File[] = []
    for (const it of Array.from(items)) {
      if (it.kind === "file") {
        const f = it.getAsFile()
        if (f) files.push(f)
      }
    }
    if (files.length > 0) {
      e.preventDefault()
      addFiles(files)
    }
  }, [addFiles])

  return { attachments, dragHover, addFiles, removeAttachment, clearAttachments, onDragEnter, onDragOver, onDragLeave, onDrop, onPaste }
}
