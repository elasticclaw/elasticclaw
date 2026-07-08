"use client"

import { useState } from "react"
import { AlertTriangle, Check, Copy, Sparkles, Trash2, Zap } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import type { LLMModelOption, SettingsData } from "./types"
import { useModelAuthLogin } from "./use-model-auth-login"

const PROVIDER_OPTIONS = [
  { value: "anthropic",  label: "Anthropic",  placeholder: "sk-ant-..." },
  { value: "fireworks",  label: "Fireworks",  placeholder: "fw_..." },
  { value: "openai",     label: "OpenAI",     placeholder: "sk-proj-..." },
  { value: "codex",      label: "Codex",      placeholder: "sk-proj-..." },
  { value: "grok",       label: "Grok Build", placeholder: "xai-..." },
  { value: "ollama",     label: "Ollama",     placeholder: "ollama-local" },
  { value: "other",      label: "Other",      placeholder: "" },
]

const PROVIDER_MODELS: Record<string, LLMModelOption[]> = {
  anthropic: [
    { id: "anthropic/claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    { id: "anthropic/claude-opus-4-5",   name: "Claude Opus 4.5" },
    { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
    { id: "__custom",                    name: "Custom Anthropic model" },
  ],
  fireworks: [
    { id: "fireworks/accounts/fireworks/models/kimi-k2p7",                  name: "Kimi K2.7" },
    { id: "fireworks/accounts/fireworks/models/glm-5p2",                    name: "GLM 5.2" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-pro",            name: "DeepSeek V4 Pro" },
    { id: "fireworks/accounts/fireworks/models/deepseek-v4-flash",          name: "DeepSeek V4 Flash" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p7",               name: "MiniMax M2.7" },
    { id: "fireworks/accounts/fireworks/models/qwen3p6-plus",               name: "Qwen3.6 Plus" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-120b",               name: "OpenAI gpt-oss-120b" },
    { id: "fireworks/accounts/fireworks/models/gpt-oss-20b",                name: "OpenAI gpt-oss-20b" },
    { id: "fireworks/accounts/fireworks/models/minimax-m2p5",               name: "MiniMax M2.5" },
    { id: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct",    name: "Llama 3.3 70B Instruct" },
    { id: "__custom",                                                       name: "Custom Fireworks model" },
  ],
  openai: [
    { id: "openai/gpt-5.5",      name: "GPT-5.5" },
    { id: "openai/gpt-5.5-pro",  name: "GPT-5.5 Pro" },
    { id: "openai/gpt-5.4",      name: "GPT-5.4" },
    { id: "openai/gpt-5.4-pro",  name: "GPT-5.4 Pro" },
    { id: "openai/gpt-5.4-mini", name: "GPT-5.4 Mini" },
    { id: "openai/o4-mini",      name: "o4-mini" },
    { id: "openai/o3",           name: "o3" },
    // coding-tuned variants available via regular OpenAI API
    { id: "openai/gpt-5.3-codex", name: "GPT-5.3 Codex (coding tuned)" },
    { id: "__custom",             name: "Custom OpenAI model" },
  ],
  codex: [
    { id: "codex/gpt-5.5",      name: "GPT-5.5" },
    { id: "codex/gpt-5.5-high", name: "GPT-5.5 High" },
    { id: "codex/gpt-5.4",      name: "GPT-5.4" },
    { id: "__custom",           name: "Custom Codex model" },
  ],
  grok: [
    { id: "grok/grok-build-0.1", name: "Grok Build" },
    { id: "grok/grok-4.3",       name: "Grok 4.3" },
    { id: "__custom",            name: "Custom Grok model" },
  ],
  ollama: [
    { id: "ollama/qwen2.5-coder:1.5b", name: "Qwen2.5 Coder 1.5B" },
    { id: "ollama/qwen2.5-coder:7b", name: "Qwen2.5 Coder 7B" },
    { id: "ollama/llama3.2:3b",      name: "Llama 3.2 3B" },
    { id: "ollama/gpt-oss:20b",      name: "gpt-oss 20B" },
    { id: "__custom",                name: "Custom Ollama model" },
  ],
}

export default function LLMSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const llmKeys = settings.llmKeys || []
  const [showModal, setShowModal] = useState(false)
  const [modalMode, setModalMode] = useState<"add" | "edit">("add")
  const [editIdx, setEditIdx] = useState<number | null>(null)

  // Form state
  const [formName, setFormName] = useState("")
  const [formProvider, setFormProvider] = useState("anthropic")
  const [formCustomProvider, setFormCustomProvider] = useState("")
  const [formKey, setFormKey] = useState("")
  const [formDefault, setFormDefault] = useState(false)
  const [formDefaultModel, setFormDefaultModel] = useState("")
  const [formCustomModel, setFormCustomModel] = useState("")
  const [formAuthProfile, setFormAuthProfile] = useState("")
  const { loginJob, loginError, copiedLoginCode, startLogin, copyLoginCode, resetLogin } = useModelAuthLogin()

  const providerLabel = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.label ?? p
  const providerPlaceholder = (p: string) => PROVIDER_OPTIONS.find(o => o.value === p)?.placeholder ?? ""
  const providerModels = (p: string) => settings.modelOptions?.[p] ?? PROVIDER_MODELS[p] ?? []
  const supportsCLIAuth = formProvider === "codex" || formProvider === "grok"
  const authProfiles = (settings.modelAuthProfiles || []).filter(p => p.provider === formProvider)

  const resetForm = () => {
    setFormName(""); setFormProvider("anthropic"); setFormCustomProvider(""); setFormKey(""); setFormDefault(false); setFormDefaultModel(""); setFormCustomModel(""); setFormAuthProfile(""); resetLogin(); setEditIdx(null)
  }

  const openAdd = () => { resetForm(); setModalMode("add"); setShowModal(true) }
  const openEdit = (i: number) => {
    const k = llmKeys[i]
    setFormName(k.name)
    const isCustom = !PROVIDER_OPTIONS.some(o => o.value === k.provider)
    setFormProvider(isCustom ? "other" : k.provider)
    setFormCustomProvider(isCustom ? k.provider : "")
    setFormKey("")
    setFormDefault(k.default)
    setFormAuthProfile(k.authProfile || "")
    const options = providerModels(k.provider)
    if (k.defaultModel && options.length > 0 && !options.some(m => m.id === k.defaultModel)) {
      setFormDefaultModel("__custom")
      setFormCustomModel(k.defaultModel)
    } else {
      setFormDefaultModel(k.defaultModel || "")
      setFormCustomModel("")
    }
    setEditIdx(i)
    setModalMode("edit")
    setShowModal(true)
  }

  const needsAttention = llmKeys.filter(k => !k.keySet).length
  const configuredCount = llmKeys.filter(k => k.keySet).length

  function doSave() {
    const actualProvider = formProvider === "other" ? formCustomProvider : formProvider
    const actualDefaultModel = formDefaultModel === "__custom" ? formCustomModel.trim() : formDefaultModel
    const actualAuthProfile = supportsCLIAuth ? formAuthProfile.trim() : ""
    if (modalMode === "add") {
      if (!formName.trim() || (!formKey.trim() && actualProvider !== "ollama" && !actualAuthProfile)) return
      onSave({ llmKeys: [{ name: formName.trim(), provider: actualProvider, apiKey: formKey.trim(), default: formDefault, defaultModel: actualDefaultModel || undefined, authProfile: actualAuthProfile || undefined }] })
    } else if (editIdx !== null) {
      const existing = llmKeys[editIdx]
      const patch: Record<string, unknown> = {
        name: existing.name,          // lookup key — original name
        newName: formName.trim(),      // new name if changed
      }
      if (formKey.trim()) patch.apiKey = formKey.trim()
      patch.default = formDefault       // always send so user can unset
      if (actualDefaultModel) patch.defaultModel = actualDefaultModel
      patch.authProfile = actualAuthProfile
      if (actualProvider !== existing.provider) patch.provider = actualProvider
      onSave({ llmKeys: [patch] })
    }
    setShowModal(false)
    resetForm()
  }

  function doRemove(i: number) {
    onSave({ llmKeys: [{ name: llmKeys[i].name, delete: true }] })
    setShowModal(false)
    resetForm()
  }

  function setDefault(i: number) {
    onSave({ llmKeys: [{ name: llmKeys[i].name, default: true }] })
  }

  async function doStartLogin() {
    const profile = formAuthProfile.trim() || `${formProvider}-default`
    setFormAuthProfile(profile)
    await startLogin(formProvider, profile)
  }

  const formProviderModels = providerModels(formProvider)

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Models</h2>
        <p className="text-sm text-muted-foreground mb-4">
          API keys for LLM providers. The default key is used unless overridden by a workflow.
        </p>

        <div className="flex items-center gap-2 mb-6">
          <span className="text-xs bg-muted text-muted-foreground px-2 py-1 rounded font-medium">
            {llmKeys.length} key{llmKeys.length !== 1 ? "s" : ""} configured
          </span>
          {needsAttention > 0 && (
            <span className="text-xs bg-red-500/10 text-red-400 border border-red-500/20 px-2 py-1 rounded font-medium flex items-center gap-1">
              <AlertTriangle className="size-3" /> {needsAttention} need{needsAttention !== 1 ? "" : "s"} attention
            </span>
          )}
        </div>
      </div>

      {/* Needs Attention */}
      {needsAttention > 0 && (
        <div>
          <p className="text-sm font-medium text-yellow-400 mb-2 flex items-center gap-1.5">
            <AlertTriangle className="size-4" /> Needs Attention
          </p>
          <div className="space-y-2">
            {llmKeys.filter(k => !k.keySet).map((k) => (
              <div
                key={k.name}
                onClick={() => openEdit(llmKeys.indexOf(k))}
                className="border border-red-500/20 rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-red-500/5 transition-colors"
              >
                <div className="flex items-center gap-3">
                  <div className="w-8 h-8 rounded-lg bg-muted flex items-center justify-center">
                    <Zap className="size-4 text-muted-foreground" />
                  </div>
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{k.name}</span>
                      <span className="text-xs text-muted-foreground">{providerLabel(k.provider)}</span>
                    </div>
                    <div className="flex items-center gap-2 mt-0.5">
                      {k.defaultModel ? (
                        <span className="text-xs text-muted-foreground">model: {k.defaultModel}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">using provider default</span>
                      )}
                      <span className="text-xs text-red-400 flex items-center gap-1">✕ Invalid</span>
                    </div>
                  </div>
                </div>
                <span className="text-muted-foreground text-lg">⋯</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Configured */}
      {configuredCount > 0 && (
        <div>
          <p className="text-sm font-medium text-muted-foreground mb-2">Configured</p>
          <div className="space-y-2">
            {llmKeys.filter(k => k.keySet).map((k) => (
              <div
                key={k.name}
                onClick={() => openEdit(llmKeys.indexOf(k))}
                className="border border-border rounded-lg p-3 flex items-center justify-between cursor-pointer hover:bg-muted/50 transition-colors"
              >
                <div className="flex items-center gap-3">
                  <div className="w-8 h-8 rounded-lg bg-muted flex items-center justify-center">
                    {k.provider === "anthropic" || k.provider === "openai" || k.provider === "codex" ? (
                      <Sparkles className="size-4 text-muted-foreground" />
                    ) : (
                      <Zap className="size-4 text-muted-foreground" />
                    )}
                  </div>
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{k.name}</span>
                      <span className="text-xs text-muted-foreground">{providerLabel(k.provider)}</span>
                      {k.default && (
                        <span className="text-xs bg-primary/20 text-primary px-1.5 py-0.5 rounded flex items-center gap-1">
                          <Sparkles className="size-3" /> Default
                        </span>
                      )}
                    </div>
                    <div className="flex items-center gap-2 mt-0.5">
                      {k.defaultModel ? (
                        <span className="text-xs text-muted-foreground">model: {k.defaultModel}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">using provider default</span>
                      )}
                      <span className="text-xs text-green-400 flex items-center gap-1">✓ Valid</span>
                    </div>
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  {!k.default && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 text-xs"
                      onClick={(e) => { e.stopPropagation(); setDefault(llmKeys.indexOf(k)) }}
                      disabled={saving}
                    >
                      Set default
                    </Button>
                  )}
                  <span className="text-muted-foreground text-lg">⋯</span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      <Button onClick={openAdd} className="gap-2">
        <span className="text-sm">+</span> Add Model Key
      </Button>

      {/* Modal */}
      <Dialog open={showModal} onOpenChange={open => { setShowModal(open); if (!open) resetForm() }}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
          <DialogTitle className="sr-only">{modalMode === "add" ? "Add Model Key" : `Edit ${formName}`}</DialogTitle>
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <h3 className="font-medium">{modalMode === "add" ? "Add Model Key" : `Edit ${formName}`}</h3>
          </div>

          <div className="p-5 space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Name</label>
                  <Input value={formName} onChange={e => setFormName(e.target.value)} className="h-8 text-sm" placeholder="anthropic-prod" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Provider</label>
                  <select
                    value={formProvider}
                    onChange={e => { setFormProvider(e.target.value); setFormDefaultModel(""); setFormCustomModel("") }}
                    className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm"
                  >
                    {PROVIDER_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                  </select>
                </div>
              </div>
              {formProvider === "other" && (
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Custom Provider Name</label>
                  <Input value={formCustomProvider} onChange={e => setFormCustomProvider(e.target.value)} className="h-8 text-sm" placeholder="mistral" />
                </div>
              )}
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
                <Input
                  type="password"
                  value={formKey}
                  onChange={e => setFormKey(e.target.value)}
                  className="h-8 text-sm"
                  placeholder={modalMode === "edit" ? "Leave blank to keep existing" : providerPlaceholder(formProvider)}
                />
              </div>
              {supportsCLIAuth && (
                <div className="space-y-2">
                  <label className="text-xs text-muted-foreground mb-1 block">CLI auth profile <span className="text-muted-foreground/60">(optional)</span></label>
                  <div className="flex gap-2">
                    <Input
                      value={formAuthProfile}
                      onChange={e => setFormAuthProfile(e.target.value)}
                      className="h-8 text-sm"
                      placeholder={`${formProvider}-default`}
                    />
                    <Button type="button" size="sm" variant="outline" onClick={doStartLogin} disabled={saving}>
                      Login
                    </Button>
                  </div>
                  {authProfiles.length > 0 && (
                    <select
                      value={formAuthProfile}
                      onChange={e => setFormAuthProfile(e.target.value)}
                      className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
                    >
                      <option value="">No CLI auth profile</option>
                      {authProfiles.map(p => (
                        <option key={p.name} value={p.name}>{p.name}{p.authenticated ? " (authenticated)" : ""}</option>
                      ))}
                    </select>
                  )}
                  {loginJob && (
                    <div className="rounded-md border border-border p-3 text-xs space-y-1">
                      <div className="text-muted-foreground">Login status: {loginJob.status}</div>
                      {loginJob.url && (
                        <a href={loginJob.url} target="_blank" rel="noopener noreferrer" className="underline break-all">{loginJob.url}</a>
                      )}
                      {loginJob.code && (
                        <div className="rounded-md border border-primary/30 bg-primary/10 p-2">
                          <div className="flex items-center justify-between gap-3">
                            <div className="min-w-0">
                              <div className="text-[11px] uppercase text-muted-foreground">One-time code</div>
                              <div className="font-mono text-lg font-semibold tracking-wide text-foreground">{loginJob.code}</div>
                            </div>
                            <Button type="button" size="sm" variant="outline" className="h-8 shrink-0 gap-1.5" onClick={() => copyLoginCode(loginJob.code || "")}>
                              {copiedLoginCode ? <Check className="size-3.5 text-green-500" /> : <Copy className="size-3.5" />}
                              {copiedLoginCode ? "Copied" : "Copy"}
                            </Button>
                          </div>
                        </div>
                      )}
                      {!loginJob.code && loginJob.output && (
                        <pre className="max-h-24 overflow-auto whitespace-pre-wrap break-words rounded border border-border bg-background/60 p-2 text-[11px] text-muted-foreground">{loginJob.output}</pre>
                      )}
                      {loginJob.error && <div className="text-red-400">{loginJob.error}</div>}
                      {loginJob.status === "complete" && <div className="text-green-400">Profile saved. Save this model key to use it.</div>}
                    </div>
                  )}
                  {loginError && <div className="text-xs text-red-400">{loginError}</div>}
                </div>
              )}
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default model <span className="text-muted-foreground/60">(optional)</span></label>
                {formProviderModels.length > 0 ? (
                  <select
                    value={formDefaultModel}
                    onChange={e => setFormDefaultModel(e.target.value)}
                    className="h-8 text-sm w-full rounded-md border border-input bg-background px-2 py-1"
                  >
                    <option value="">— use provider default —</option>
                    {formProviderModels.map(m => (
                      <option key={m.id} value={m.id}>{m.name}</option>
                    ))}
                  </select>
                ) : (
                  <Input value={formDefaultModel} onChange={e => setFormDefaultModel(e.target.value)} className="h-8 text-sm" placeholder="e.g. myprovider/model-name" />
                )}
                {formProviderModels.length > 0 && formDefaultModel === "__custom" && (
                  <Input
                    value={formCustomModel}
                    onChange={e => setFormCustomModel(e.target.value)}
                    className="h-8 text-sm mt-2"
                    placeholder={formProvider === "ollama" ? "e.g. ollama/qwen3:14b" : "e.g. provider/model-name"}
                  />
                )}
              </div>
              <label className="flex items-center gap-2 text-sm cursor-pointer">
                <input type="checkbox" checked={formDefault} onChange={e => setFormDefault(e.target.checked)} />
                Set as default key
              </label>
            </div>

            <div className="flex items-center justify-between px-5 py-4 border-t border-border">
              {modalMode === "edit" && editIdx !== null && (
                <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => doRemove(editIdx)}>
                  <Trash2 className="size-3.5 mr-1" /> Remove
                </Button>
              )}
              <div className="flex items-center gap-2 ml-auto">
                <Button size="sm" variant="outline" onClick={() => { setShowModal(false); resetForm() }}>Cancel</Button>
                <Button
                  size="sm"
                  disabled={saving || !formName.trim() || (modalMode === "add" && !formKey.trim() && formProvider !== "ollama" && !formAuthProfile.trim()) || (formProvider === "other" && !formCustomProvider.trim())}
                  onClick={doSave}
                >
                  {modalMode === "add" ? "Add Model Key" : "Save changes"}
                </Button>
              </div>
            </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
