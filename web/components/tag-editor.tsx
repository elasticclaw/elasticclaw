"use client"

import { useState, useRef, useEffect } from "react"
import { X, Plus } from "lucide-react"
import { cn } from "@/lib/utils"
import { patchClaw } from "@/lib/api"
import { toast } from "sonner"

interface TagEditorProps {
  clawId: string
  tags: string[]
  onTagsChange: (tags: string[]) => void
  className?: string
}

export function TagEditor({ clawId, tags, onTagsChange, className }: TagEditorProps) {
  const [adding, setAdding] = useState(false)
  const [inputValue, setInputValue] = useState("")
  const [saving, setSaving] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (adding) inputRef.current?.focus()
  }, [adding])

  async function removeTag(tag: string) {
    const next = tags.filter((t) => t !== tag)
    setSaving(true)
    try {
      await patchClaw(clawId, { tags: next })
      onTagsChange(next)
    } catch (e) {
      console.error("Failed to remove tag", e)
      toast.error("Failed to remove tag")
    } finally {
      setSaving(false)
    }
  }

  async function addTag() {
    const tag = inputValue.trim()
    if (!tag || tags.includes(tag)) {
      setInputValue("")
      setAdding(false)
      return
    }
    const next = [tag, ...tags]
    setSaving(true)
    try {
      await patchClaw(clawId, { tags: next })
      onTagsChange(next)
    } catch (e) {
      console.error("Failed to add tag", e)
      toast.error("Failed to add tag")
    } finally {
      setSaving(false)
      setInputValue("")
      setAdding(false)
    }
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Enter") addTag()
    if (e.key === "Escape") {
      setInputValue("")
      setAdding(false)
    }
  }

  const MAX_VISIBLE = 3
  const visibleTags = tags.slice(0, MAX_VISIBLE)
  const hiddenCount = tags.length - MAX_VISIBLE

  return (
    <div className={cn("flex flex-wrap items-center gap-1 overflow-hidden", className)}>
      {visibleTags.map((tag) => (
        <span
          key={tag}
          title={tag}
          className="inline-flex items-center gap-0.5 px-1.5 py-0.5 text-[10px] font-medium bg-secondary text-muted-foreground rounded group/tag max-w-[120px]"
        >
          <span className="truncate">{tag}</span>
          <button
            onClick={() => removeTag(tag)}
            disabled={saving}
            className="ml-0.5 opacity-0 group-hover/tag:opacity-100 hover:text-destructive transition-opacity flex-shrink-0"
            title="Remove tag"
          >
            <X className="size-2.5" />
          </button>
        </span>
      ))}
      {hiddenCount > 0 && (
        <span
          title={tags.slice(MAX_VISIBLE).join(", ")}
          className="inline-flex items-center px-1.5 py-0.5 text-[10px] font-medium bg-secondary text-muted-foreground rounded cursor-default"
        >
          +{hiddenCount} more
        </span>
      )}

      {adding ? (
        <input
          ref={inputRef}
          value={inputValue}
          onChange={(e) => setInputValue(e.target.value)}
          onKeyDown={handleKeyDown}
          onBlur={addTag}
          placeholder="tag name"
          className="h-5 w-24 px-1.5 text-[10px] bg-secondary border border-border rounded outline-none focus:border-ring"
          disabled={saving}
        />
      ) : (
        <button
          onClick={() => setAdding(true)}
          className="inline-flex items-center gap-0.5 px-1.5 py-0.5 text-[10px] text-muted-foreground hover:text-foreground hover:bg-secondary rounded transition-colors"
          title="Add tag"
        >
          <Plus className="size-2.5" />
          <span>tag</span>
        </button>
      )}
    </div>
  )
}
