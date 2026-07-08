"use client"

import { Eye, EyeOff, RotateCcw, Send } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"
import { renderMarkdown, stripYamlBlocks } from "./ai-config-markdown"
import { useAIConfigChat } from "./use-ai-config-chat"
import { YamlHighlight } from "./yaml-highlight"

export default function AIConfigSection() {
  const {
    messages, input, setInput, loading, error,
    proposedYaml, currentConfig, placeholders, secretValues, setSecretValues,
    backupPath, applying, reverting, applySuccess,
    yamlStreaming, streamingYaml,
    revealSecrets, setRevealSecrets,
    chatScrollRef, chatInputRef,
    sendMessage, applyConfig, revertConfig, startOver, discardProposed,
  } = useAIConfigChat()

  const allPlaceholdersFilled = placeholders.every(p => secretValues[p]?.trim())
  const displayedYaml = yamlStreaming ? streamingYaml : (proposedYaml ?? currentConfig)
  const yamlLabel = yamlStreaming ? "Generating config…" : (proposedYaml ? "Proposed config" : "Current config")

  return (
    <div className="flex flex-col" style={{ height: "calc(100vh - 8rem)" }}>
      {/* Header */}
      <div className="px-8 pt-6 pb-3 flex-none flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold mb-0.5">Configure with AI</h2>
          <p className="text-sm text-muted-foreground">
            Describe changes in plain English. The AI will propose a hub.yaml update for you to review and apply.
          </p>
        </div>
        {messages.length > 0 && (
          <Button
            variant="ghost"
            size="sm"
            className="text-muted-foreground shrink-0"
            onClick={startOver}
          >
            <RotateCcw className="size-3.5 mr-1.5" />
            Start over
          </Button>
        )}
      </div>

      {/* Status bar */}
      {(error || applySuccess || (backupPath && !applySuccess)) && (
        <div className="px-8 flex-none mb-2">
          {error && (
            <p className="text-sm text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>
          )}
          {applySuccess && (
            <div className="flex items-center gap-3">
              <p className="text-sm text-green-600">&check; Config applied.</p>
              {backupPath && (
                <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
                  <RotateCcw className="size-3.5 mr-1" />
                  {reverting ? "Reverting…" : "Revert"}
                </Button>
              )}
            </div>
          )}
          {backupPath && !applySuccess && (
            <div className="flex items-center gap-3">
              <p className="text-xs text-muted-foreground">Previous backup available.</p>
              <Button size="sm" variant="outline" onClick={revertConfig} disabled={reverting}>
                <RotateCcw className="size-3.5 mr-1" />
                {reverting ? "Reverting…" : "Revert to backup"}
              </Button>
            </div>
          )}
        </div>
      )}

      {/* Two-column body — fills remaining height */}
      <div className="flex flex-1 min-h-0 px-8 pb-6 gap-4">

        {/* Left: chat */}
        <div className="flex flex-col flex-1 min-w-0 min-h-0 border border-border rounded-lg bg-muted/10 overflow-hidden">
          {/* Scrollable message history */}
          <div ref={chatScrollRef} className="flex-1 overflow-y-auto p-4 space-y-3">
            {messages.length === 0 && (
              <p className="text-sm text-muted-foreground text-center py-8">
                Describe the config change you&apos;d like to make.
              </p>
            )}
            {messages.map((m, i) => (
              <div key={i} className={cn("flex", m.role === "user" ? "justify-end" : "justify-start")}>
                <div
                  className={cn(
                    "max-w-[90%] rounded-xl px-3 py-2 text-sm break-words",
                    m.role === "user"
                      ? "bg-primary text-primary-foreground whitespace-pre-wrap"
                      : "bg-muted text-foreground"
                  )}
                >
                  {m.role === "assistant"
                    ? <span>{renderMarkdown(stripYamlBlocks(m.content.replace(/▌$/, "")))}{m.streaming && <span className="animate-pulse">&#x258c;</span>}</span>
                    : m.content
                  }
                </div>
              </div>
            ))}
            {loading && (messages.length === 0 || messages[messages.length - 1]?.role === "user") && (
              <div className="flex justify-start">
                <div className="bg-muted rounded-xl px-3 py-2 text-sm text-muted-foreground">
                  <span className="inline-flex gap-1">
                    <span className="animate-bounce" style={{ animationDelay: "0ms" }}>&middot;</span>
                    <span className="animate-bounce" style={{ animationDelay: "150ms" }}>&middot;</span>
                    <span className="animate-bounce" style={{ animationDelay: "300ms" }}>&middot;</span>
                  </span>
                </div>
              </div>
            )}
          </div>

          {/* Input pinned to bottom */}
          <div className="flex-none border-t border-border p-3 flex gap-2">
            <Input
              ref={chatInputRef}
              placeholder="e.g. Add a Linear integration for workspace acme"
              value={input}
              onChange={e => setInput(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendMessage() } }}
              disabled={loading}
              className="flex-1 text-sm"
              autoFocus
            />
            <Button size="icon" onClick={sendMessage} disabled={loading || !input.trim()}>
              <Send className="size-4" />
            </Button>
          </div>
        </div>

        {/* Right: config panel */}
        <div className="flex flex-col min-h-0 gap-3 flex-1 min-w-0">
          {/* Label + secret toggle */}
          <div className="flex-none flex items-center justify-between gap-2">
            <div className="flex items-center gap-2">
              <span className={cn(
                "text-xs font-medium uppercase tracking-wide px-2 py-0.5 rounded",
                yamlStreaming
                  ? "bg-blue-500/15 text-blue-500 dark:text-blue-400"
                  : proposedYaml
                    ? "bg-amber-500/15 text-amber-600 dark:text-amber-400"
                    : "bg-muted text-muted-foreground"
              )}>
                {yamlLabel}
              </span>
              {yamlStreaming && (
                <span className="flex gap-0.5 items-center">
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:0ms]" />
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:150ms]" />
                  <span className="size-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:300ms]" />
                </span>
              )}
            </div>
            <div className="flex items-center gap-2">
              {revealSecrets && (
                <span className="text-xs text-amber-500 font-medium">Secrets visible</span>
              )}
              <Button
                size="icon"
                variant="ghost"
                className="size-6"
                title={revealSecrets ? "Hide secrets" : "Reveal secrets"}
                onClick={() => setRevealSecrets(v => !v)}
              >
                {revealSecrets ? <Eye className="size-3.5" /> : <EyeOff className="size-3.5" />}
              </Button>
            </div>
          </div>

          {/* YAML display — fills available height, scrollable */}
          <div className={cn(
            "flex-1 min-h-0 border rounded-lg overflow-hidden bg-[#0d1117] relative transition-colors duration-300",
            yamlStreaming ? "border-blue-500/50" : "border-border"
          )}>
            {yamlStreaming
              ? <pre className="h-full overflow-auto p-3 text-xs font-mono leading-relaxed text-gray-300 whitespace-pre">{streamingYaml}<span className="animate-pulse text-amber-400">&#x258c;</span></pre>
              : displayedYaml
                ? <YamlHighlight code={displayedYaml} />
                : <p className="p-3 text-xs text-muted-foreground">Loading…</p>
            }
          </div>

          {/* Placeholder secret inputs */}
          {proposedYaml && placeholders.length > 0 && (
            <div className="flex-none border border-border rounded-lg p-3 space-y-2 bg-background">
              <p className="text-xs font-medium text-muted-foreground">Fill in secrets</p>
              {placeholders.map(ph => (
                <div key={ph}>
                  <label className="text-xs text-muted-foreground font-mono mb-1 block">{ph}</label>
                  <Input
                    type="password"
                    placeholder={`Value for ${ph}`}
                    value={secretValues[ph] || ""}
                    onChange={e => setSecretValues(prev => ({ ...prev, [ph]: e.target.value }))}
                    className="font-mono text-xs h-7"
                  />
                </div>
              ))}
            </div>
          )}

          {/* Apply / Discard buttons */}
          {proposedYaml && (
            <div className="flex-none flex gap-2">
              <Button
                onClick={applyConfig}
                disabled={applying || !allPlaceholdersFilled}
                className="flex-1"
              >
                {applying ? "Applying…" : "Apply"}
              </Button>
              <Button
                variant="outline"
                disabled={applying}
                onClick={discardProposed}
              >
                Discard
              </Button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
