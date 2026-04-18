"use client"

import { useState } from "react"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { Plus, X } from "lucide-react"

interface SpawnModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSpawn: (template: string, name?: string, tags?: Record<string, string>) => void
}

export function SpawnModal({ open, onOpenChange, onSpawn }: SpawnModalProps) {
  const [template, setTemplate] = useState("")
  const [name, setName] = useState("")
  const [tags, setTags] = useState<Array<{ key: string; value: string }>>([])
  const [newTagKey, setNewTagKey] = useState("")
  const [newTagValue, setNewTagValue] = useState("")

  const handleSpawn = () => {
    if (!template.trim()) return
    const tagsObj = tags.reduce((acc, tag) => {
      if (tag.key.trim() && tag.value.trim()) {
        acc[tag.key.trim()] = tag.value.trim()
      }
      return acc
    }, {} as Record<string, string>)
    onSpawn(template.trim(), name.trim() || undefined, Object.keys(tagsObj).length > 0 ? tagsObj : undefined)
    setTemplate("")
    setName("")
    setTags([])
    setNewTagKey("")
    setNewTagValue("")
  }

  const handleAddTag = () => {
    if (newTagKey.trim() && newTagValue.trim()) {
      setTags([...tags, { key: newTagKey.trim(), value: newTagValue.trim() }])
      setNewTagKey("")
      setNewTagValue("")
    }
  }

  const handleRemoveTag = (index: number) => {
    setTags(tags.filter((_, i) => i !== index))
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && template.trim()) {
      e.preventDefault()
      handleSpawn()
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Spawn New Claw</DialogTitle>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <Field>
            <FieldLabel htmlFor="template">Template</FieldLabel>
            <Input
              id="template"
              placeholder="github.com/marccampbell/support-claw"
              value={template}
              onChange={(e) => setTemplate(e.target.value)}
              onKeyDown={handleKeyDown}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="name">
              Name <span className="text-muted-foreground">(optional)</span>
            </FieldLabel>
            <Input
              id="name"
              placeholder="Auto-generated if blank"
              value={name}
              onChange={(e) => setName(e.target.value)}
              onKeyDown={handleKeyDown}
            />
          </Field>
          <Field>
            <FieldLabel>
              Tags <span className="text-muted-foreground">(optional)</span>
            </FieldLabel>
            <div className="space-y-2">
              {tags.length > 0 && (
                <div className="flex flex-wrap gap-1.5">
                  {tags.map((tag, index) => (
                    <span
                      key={index}
                      className="inline-flex items-center gap-1 px-2 py-1 text-xs bg-secondary text-foreground rounded"
                    >
                      <span className="text-muted-foreground">{tag.key}=</span>
                      {tag.value}
                      <button
                        onClick={() => handleRemoveTag(index)}
                        className="ml-0.5 hover:text-destructive"
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
              <div className="flex items-center gap-2">
                <Input
                  placeholder="key"
                  value={newTagKey}
                  onChange={(e) => setNewTagKey(e.target.value)}
                  className="flex-1 h-8 text-sm"
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault()
                      handleAddTag()
                    }
                  }}
                />
                <span className="text-muted-foreground">=</span>
                <Input
                  placeholder="value"
                  value={newTagValue}
                  onChange={(e) => setNewTagValue(e.target.value)}
                  className="flex-1 h-8 text-sm"
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault()
                      handleAddTag()
                    }
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="icon"
                  className="size-8 shrink-0"
                  onClick={handleAddTag}
                  disabled={!newTagKey.trim() || !newTagValue.trim()}
                >
                  <Plus className="size-4" />
                </Button>
              </div>
            </div>
          </Field>
        </div>
        <DialogFooter>
          <Button
            onClick={handleSpawn}
            disabled={!template.trim()}
          >
            Spawn
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
