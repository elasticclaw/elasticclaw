"use client"

import { useState, useRef, useEffect } from "react"
import { X, Plus } from "lucide-react"
import { cn } from "@/lib/utils"
import { patchClaw } from "@/lib/api"

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
    } finally {
      setSaving(false)
    }
  }

  async function addTag() {
    const raw = inputValue.trim()
    if (!raw) {
      setAdding(false)
      return
    }
    // Normalize to k=v
    const tag = raw.includes("=") ? raw : `${raw}=true`
    if (tags.includes(tag)) {
      setInputValue("")
      setAdding(false)
      return
    }
    const next = [...tags, tag]
    setSaving(true)
    try {
      await patchClaw(clawId, { tags: next })
      onTagsChange(next)
    } catch (e) {
      console.error("Failed to add tag", e)
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

  return (
    <div className={cn("flex flex-wrap items-center gap-1", className)}>
      {tags.map((tag) => {
        const [key, value] = tag.split("=", 2)
        return (
          <span
            key={tag}
            className="inline-flex items-center gap-0.5 px-1.5 py-0.5 text-[10px] font-medium bg-secondary text-muted-foreground rounded group/tag"
          >
            <span className="opacity-60">{key}=</span>
            <span className="text-foreground/80">{value}</span>
            <button
              onClick={() => removeTag(tag)}
              disabled={saving}
              className="ml-0.5 opacity-0 group-hover/tag:opacity-100 hover:text-destructive transition-opacity"
              title="Remove tag"
            >
              <X className="size-2.5" />
            </button>
          </span>
        )
      })}

      {adding ? (
        <input
          ref={inputRef}
          value={inputValue}
          onChange={(e) => setInputValue(e.target.value)}
          onKeyDown={handleKeyDown}
          onBlur={addTag}
          placeholder="key=value"
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
