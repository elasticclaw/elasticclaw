"use client"

import { useEffect, useState } from "react"
import { Check, Copy, Trash2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import type { SettingsData } from "./types"

export const SANDBOX_PROVIDER_OPTIONS = [
  { value: "replicated", label: "Replicated CMX", description: "Kubernetes-based VM provider" },
  { value: "daytona", label: "Daytona", description: "Development environment provider" },
  { value: "exedev", label: "exe.dev", description: "Persistent VM provider with SSH access" },
  { value: "docker", label: "Local Docker", description: "Local Docker daemon provider for development and testing" },
  { value: "lambda-microvms", label: "AWS Lambda MicroVMs", description: "Serverless Firecracker MicroVM provider", alpha: true },
]

export function ProviderModal({ open, mode, editName, providers, saving, onSave, onClose }: {
  open: boolean
  mode: "add" | "edit"
  editName: string | null
  providers: SettingsData["providers"]
  saving: boolean
  onSave: (p: object) => void
  onClose: () => void
}) {
  const [copiedKey, setCopiedKey] = useState(false)

  // Form state
  const [formProvider, setFormProvider] = useState("replicated")
  const [formToken, setFormToken] = useState("")
  const [formApiUrl, setFormApiUrl] = useState("")
  const [formApiKey, setFormApiKey] = useState("")
  const [formDefaultTtl, setFormDefaultTtl] = useState("")
  const [formDefaultInstanceType, setFormDefaultInstanceType] = useState("")
  const [formDefaultSnapshot, setFormDefaultSnapshot] = useState("")
  const [formDefaultCpu, setFormDefaultCpu] = useState("")
  const [formDefaultMemory, setFormDefaultMemory] = useState("")
  const [formDefaultDisk, setFormDefaultDisk] = useState("")
  const [formDockerImage, setFormDockerImage] = useState("")
  const [formDockerNetwork, setFormDockerNetwork] = useState("")
  const [formAwsRegion, setFormAwsRegion] = useState("")
  const [formAwsProfile, setFormAwsProfile] = useState("")
  const [formImageIdentifier, setFormImageIdentifier] = useState("")
  const [formImageVersion, setFormImageVersion] = useState("")
  const [formExecutionRoleArn, setFormExecutionRoleArn] = useState("")
  const [formIngressConnectors, setFormIngressConnectors] = useState("")
  const [formEgressConnectors, setFormEgressConnectors] = useState("")
  const [formIdleMaxDuration, setFormIdleMaxDuration] = useState("")
  const [formSuspendedDuration, setFormSuspendedDuration] = useState("")
  const [formAutoResume, setFormAutoResume] = useState(true)
  const [formMaximumDuration, setFormMaximumDuration] = useState("")
  const [formBridgePort, setFormBridgePort] = useState("")
  const [formAuthTokenExpiration, setFormAuthTokenExpiration] = useState("")

  // (Re)initialize the form whenever the modal opens.
  useEffect(() => {
    if (!open) return
    if (mode === "edit" && editName) {
      const p = providers[editName]
      setFormProvider(editName)
      setFormToken("")
      setFormApiUrl(p?.apiUrl || "")
      setFormApiKey("")
      setFormDefaultTtl(p?.defaultTtl || "")
      setFormDefaultInstanceType(p?.defaultInstanceType || "")
      setFormDefaultSnapshot(p?.defaultSnapshot || "")
      setFormDefaultCpu(p?.defaultCpu?.toString() || "")
      setFormDefaultMemory(p?.defaultMemory ? p.defaultMemory.replace(/GB$/, "") : "")
      setFormDefaultDisk(p?.defaultDisk ? p.defaultDisk.replace(/GB$/, "") : "")
      setFormDockerImage(p?.image || "")
      setFormDockerNetwork(p?.network || "")
      setFormAwsRegion(p?.awsRegion || "")
      setFormAwsProfile(p?.awsProfile || "")
      setFormImageIdentifier(p?.imageIdentifier || "")
      setFormImageVersion(p?.imageVersion || "")
      setFormExecutionRoleArn(p?.executionRoleArn || "")
      setFormIngressConnectors((p?.ingressNetworkConnectors || []).join("\n"))
      setFormEgressConnectors((p?.egressNetworkConnectors || []).join("\n"))
      setFormIdleMaxDuration(p?.idleMaxDurationSeconds?.toString() || "900")
      setFormSuspendedDuration(p?.suspendedDurationSeconds?.toString() || "300")
      setFormAutoResume(p?.autoResume ?? true)
      setFormMaximumDuration(p?.maximumDurationSeconds?.toString() || "28800")
      setFormBridgePort(p?.bridgePort?.toString() || "8080")
      setFormAuthTokenExpiration(p?.authTokenExpirationMinutes?.toString() || "30")
    } else {
      // Add mode: reset and select the first provider that is not configured yet.
      setFormProvider("replicated")
      setFormToken("")
      setFormApiUrl("")
      setFormApiKey("")
      setFormDefaultTtl("")
      setFormDefaultInstanceType("")
      setFormDefaultSnapshot("")
      setFormDefaultCpu("")
      setFormDefaultMemory("")
      setFormDefaultDisk("")
      setFormDockerImage("")
      setFormDockerNetwork("")
      setFormAwsRegion("")
      setFormAwsProfile("")
      setFormImageIdentifier("")
      setFormImageVersion("")
      setFormExecutionRoleArn("")
      setFormIngressConnectors("")
      setFormEgressConnectors("")
      setFormIdleMaxDuration("900")
      setFormSuspendedDuration("300")
      setFormAutoResume(true)
      setFormMaximumDuration("28800")
      setFormBridgePort("8080")
      setFormAuthTokenExpiration("30")
      const firstAvailable = SANDBOX_PROVIDER_OPTIONS.find(o => !providers[o.value])
      if (firstAvailable) {
        setFormProvider(firstAvailable.value)
        if (firstAvailable.value === "exedev") {
          setFormDefaultCpu("2")
          setFormDefaultMemory("4")
          setFormDefaultDisk("10")
        }
      }
    }
    setCopiedKey(false)
  // Intentionally keyed on the open/mode/target only: the form must not be
  // clobbered by background settings refreshes while it is being edited.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, mode, editName])

  const availableProviders = SANDBOX_PROVIDER_OPTIONS.filter(o => !providers[o.value])

  function splitConnectorList(value: string) {
    return value
      .split(/[\n,]/)
      .map(v => v.trim())
      .filter(Boolean)
  }

  function setPositiveInt(patch: Record<string, unknown>, key: string, value: string) {
    const parsed = parseInt(value, 10)
    if (value && !isNaN(parsed) && parsed > 0) patch[key] = parsed
  }

  function doSave() {
    const patch: Record<string, unknown> = {}
    if (formProvider === "replicated") {
      if (formDefaultTtl) patch.defaultTtl = formDefaultTtl
      if (formDefaultInstanceType) patch.defaultInstanceType = formDefaultInstanceType
      if (formToken) patch.token = formToken
    } else if (formProvider === "daytona") {
      if (formApiUrl) patch.apiUrl = formApiUrl
      if (formApiKey) patch.apiKey = formApiKey
      if (formDefaultSnapshot) patch.defaultSnapshot = formDefaultSnapshot
    } else if (formProvider === "exedev") {
      // exe.dev uses SSH key authentication; no API key needed in config
      patch.enabled = true
      const parsedCpu = parseInt(formDefaultCpu, 10)
      if (formDefaultCpu && !isNaN(parsedCpu)) patch.defaultCpu = parsedCpu
      if (formDefaultMemory) patch.defaultMemory = formDefaultMemory + "GB"
      if (formDefaultDisk) patch.defaultDisk = formDefaultDisk + "GB"
    } else if (formProvider === "docker") {
      patch.enabled = true
      patch.image = formDockerImage.trim()
      patch.network = formDockerNetwork.trim()
    } else if (formProvider === "lambda-microvms") {
      patch.enabled = true
      if (formAwsRegion) patch.awsRegion = formAwsRegion.trim()
      if (formAwsProfile) patch.awsProfile = formAwsProfile.trim()
      if (formImageIdentifier) patch.imageIdentifier = formImageIdentifier.trim()
      if (formImageVersion) patch.imageVersion = formImageVersion.trim()
      if (formExecutionRoleArn) patch.executionRoleArn = formExecutionRoleArn.trim()
      patch.ingressNetworkConnectors = splitConnectorList(formIngressConnectors)
      patch.egressNetworkConnectors = splitConnectorList(formEgressConnectors)
      setPositiveInt(patch, "idleMaxDurationSeconds", formIdleMaxDuration)
      setPositiveInt(patch, "suspendedDurationSeconds", formSuspendedDuration)
      patch.autoResume = formAutoResume
      setPositiveInt(patch, "maximumDurationSeconds", formMaximumDuration)
      setPositiveInt(patch, "bridgePort", formBridgePort)
      setPositiveInt(patch, "authTokenExpirationMinutes", formAuthTokenExpiration)
    }
    onSave({ providers: { [formProvider]: patch } })
    onClose()
  }

  function doRemove(name: string) {
    onSave({ providers: { [name]: { delete: true } } })
    onClose()
  }

  const modalTitle = mode === "add"
    ? "Add Sandbox Provider"
    : `Edit ${SANDBOX_PROVIDER_OPTIONS.find(o => o.value === formProvider)?.label || formProvider}`

  return (
    <Dialog open={open} onOpenChange={isOpen => { if (!isOpen) onClose() }}>
      <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto p-0 gap-0">
        <DialogTitle className="sr-only">{modalTitle}</DialogTitle>
        <div className="flex items-center justify-between px-5 py-4 border-b border-border">
          <h3 className="font-medium">{modalTitle}</h3>
        </div>

        <div className="p-5 space-y-4">
          {mode === "add" && (
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Provider</label>
              <select
                value={formProvider}
                onChange={e => setFormProvider(e.target.value)}
                className="w-full h-8 rounded-md border border-border bg-background px-2 text-sm"
              >
                {availableProviders.map(o => <option key={o.value} value={o.value}>{o.label}{o.alpha ? " (alpha)" : ""}</option>)}
              </select>
            </div>
          )}

          {formProvider === "replicated" && (
            <>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Token</label>
                <Input
                  type="password"
                  value={formToken}
                  onChange={e => setFormToken(e.target.value)}
                  className="h-8 text-sm"
                  placeholder={mode === "edit" && providers.replicated?.tokenSet ? "Leave blank to keep existing" : "Enter Replicated API token"}
                />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default TTL</label>
                  <Input value={formDefaultTtl} onChange={e => setFormDefaultTtl(e.target.value)} className="h-8 text-sm" placeholder="48h" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Default Instance</label>
                  <Input value={formDefaultInstanceType} onChange={e => setFormDefaultInstanceType(e.target.value)} className="h-8 text-sm" placeholder="r1.large" />
                </div>
              </div>
            </>
          )}

          {formProvider === "daytona" && (
            <>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API URL</label>
                <Input value={formApiUrl} onChange={e => setFormApiUrl(e.target.value)} className="h-8 text-sm" placeholder="https://app.daytona.io" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">API Key</label>
                <Input
                  type="password"
                  value={formApiKey}
                  onChange={e => setFormApiKey(e.target.value)}
                  className="h-8 text-sm"
                  placeholder={mode === "edit" && providers.daytona?.apiKeySet ? "Leave blank to keep existing" : "Enter Daytona API key"}
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default Snapshot</label>
                <Input value={formDefaultSnapshot} onChange={e => setFormDefaultSnapshot(e.target.value)} className="h-8 text-sm" placeholder="daytona-medium" />
              </div>
            </>
          )}

          {formProvider === "exedev" && (
            <>
              <div className="bg-muted/50 rounded-lg p-3 space-y-2">
                <p className="text-xs font-medium">SSH Key Setup</p>
                <p className="text-xs text-muted-foreground">
                  {mode === "edit"
                    ? <>A key pair has been generated for exe.dev. Add this public key to your{" "}<a href="https://exe.dev" target="_blank" rel="noopener noreferrer" className="underline">exe.dev account</a>.</>
                    : <>An SSH key pair will be generated automatically when you save. You can copy the public key from the edit view and add it to your{" "}<a href="https://exe.dev" target="_blank" rel="noopener noreferrer" className="underline">exe.dev account</a>.</>}
                </p>
                {mode === "edit" && providers.exedev?.sshPublicKey ? (
                  <div className="flex items-center gap-2">
                    <code className="text-xs font-mono bg-background px-2 py-1 rounded flex-1 truncate">
                      {providers.exedev.sshPublicKey}
                    </code>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 px-2"
                      onClick={() => {
                        navigator.clipboard.writeText(providers.exedev.sshPublicKey || "")
                        setCopiedKey(true)
                        setTimeout(() => setCopiedKey(false), 2000)
                      }}
                    >
                      {copiedKey ? <Check className="size-3 text-green-500" /> : <Copy className="size-3" />}
                    </Button>
                  </div>
                ) : (
                  <p className="text-xs text-muted-foreground italic">
                    Public key will be shown after saving.
                  </p>
                )}
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default CPUs</label>
                <Input
                  type="number"
                  min={1}
                  max={32}
                  value={formDefaultCpu}
                  onChange={e => setFormDefaultCpu(e.target.value)}
                  className="h-8 text-sm"
                  placeholder="2"
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default Memory (GB)</label>
                <Input
                  type="number"
                  min={1}
                  max={128}
                  value={formDefaultMemory}
                  onChange={e => setFormDefaultMemory(e.target.value)}
                  className="h-8 text-sm"
                  placeholder="4"
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Default Disk (GB)</label>
                <Input
                  type="number"
                  min={10}
                  max={500}
                  value={formDefaultDisk}
                  onChange={e => setFormDefaultDisk(e.target.value)}
                  className="h-8 text-sm"
                  placeholder="10"
                />
              </div>
            </>
          )}

          {formProvider === "docker" && (
            <>
              <div className="bg-muted/50 rounded-lg p-3 space-y-2">
                <p className="text-xs font-medium">Local Docker provider</p>
                <p className="text-xs text-muted-foreground">
                  Runs agent containers with the hub host&apos;s Docker daemon. If the hub runs in a container, mount the Docker socket so it can create sibling containers.
                </p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Agent image</label>
                <Input
                  value={formDockerImage}
                  onChange={e => setFormDockerImage(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder="ghcr.io/openclaw/openclaw:2026.6.9"
                />
                <p className="text-[11px] text-muted-foreground mt-1">
                  Leave blank to use the pinned OpenClaw image, ghcr.io/openclaw/openclaw:2026.6.9.
                </p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Docker network</label>
                <Input
                  value={formDockerNetwork}
                  onChange={e => setFormDockerNetwork(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder="elasticclaw-dev"
                />
                <p className="text-[11px] text-muted-foreground mt-1">
                  Optional. Use a network where agent containers can reach the hub.
                </p>
              </div>
            </>
          )}

          {formProvider === "lambda-microvms" && (
            <>
              <div className="bg-amber-500/10 border border-amber-500/20 rounded-lg p-3">
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-xs bg-amber-500/20 text-amber-300 px-1.5 py-0.5 rounded">Alpha</span>
                  <p className="text-xs font-medium">AWS Lambda MicroVMs</p>
                </div>
                <p className="text-xs text-muted-foreground">
                  Requires an image that starts the Elastic Claw bridge from the MicroVM run hook payload.
                </p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Image identifier</label>
                <Input
                  value={formImageIdentifier}
                  onChange={e => setFormImageIdentifier(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder="arn:aws:lambda:us-east-1:123456789012:microvm-image/elasticclaw"
                />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">AWS Region</label>
                  <Input value={formAwsRegion} onChange={e => setFormAwsRegion(e.target.value)} className="h-8 text-sm" placeholder="us-east-1" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">AWS Profile</label>
                  <Input value={formAwsProfile} onChange={e => setFormAwsProfile(e.target.value)} className="h-8 text-sm" placeholder="default" />
                </div>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Image version</label>
                <Input value={formImageVersion} onChange={e => setFormImageVersion(e.target.value)} className="h-8 text-sm" placeholder="latest" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Execution role ARN</label>
                <Input
                  value={formExecutionRoleArn}
                  onChange={e => setFormExecutionRoleArn(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder="arn:aws:iam::123456789012:role/elasticclaw-microvm"
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Ingress network connectors</label>
                <textarea
                  value={formIngressConnectors}
                  onChange={e => setFormIngressConnectors(e.target.value)}
                  className="min-h-16 w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm font-mono"
                  placeholder="One ARN per line"
                />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Egress network connectors</label>
                <textarea
                  value={formEgressConnectors}
                  onChange={e => setFormEgressConnectors(e.target.value)}
                  className="min-h-16 w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm font-mono"
                  placeholder="One ARN per line"
                />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Idle max seconds</label>
                  <Input type="number" min={1} value={formIdleMaxDuration} onChange={e => setFormIdleMaxDuration(e.target.value)} className="h-8 text-sm" placeholder="900" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Suspended seconds</label>
                  <Input type="number" min={1} value={formSuspendedDuration} onChange={e => setFormSuspendedDuration(e.target.value)} className="h-8 text-sm" placeholder="300" />
                </div>
              </div>
              <div className="grid grid-cols-3 gap-3">
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Max seconds</label>
                  <Input type="number" min={1} value={formMaximumDuration} onChange={e => setFormMaximumDuration(e.target.value)} className="h-8 text-sm" placeholder="28800" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Bridge port</label>
                  <Input type="number" min={1} value={formBridgePort} onChange={e => setFormBridgePort(e.target.value)} className="h-8 text-sm" placeholder="8080" />
                </div>
                <div>
                  <label className="text-xs text-muted-foreground mb-1 block">Token minutes</label>
                  <Input type="number" min={1} value={formAuthTokenExpiration} onChange={e => setFormAuthTokenExpiration(e.target.value)} className="h-8 text-sm" placeholder="30" />
                </div>
              </div>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={formAutoResume}
                  onChange={e => setFormAutoResume(e.target.checked)}
                  className="size-4 rounded border-border"
                />
                Auto resume suspended MicroVMs
              </label>
            </>
          )}
        </div>

        <div className="flex items-center justify-between px-5 py-4 border-t border-border">
          {mode === "edit" && editName && (
            <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => doRemove(editName)}>
              <Trash2 className="size-3.5 mr-1" /> Remove
            </Button>
          )}
          <div className="flex items-center gap-2 ml-auto">
            <Button size="sm" variant="outline" onClick={onClose}>Cancel</Button>
            <Button
              size="sm"
              disabled={saving || (mode === "add" && formProvider === "replicated" && !formToken) || (mode === "add" && formProvider === "daytona" && !formApiKey) || (mode === "add" && formProvider === "exedev" && (!formDefaultCpu || !formDefaultMemory || !formDefaultDisk)) || (formProvider === "lambda-microvms" && !formImageIdentifier.trim())}
              onClick={doSave}
            >
              {mode === "add" ? "Add Provider" : "Save changes"}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
