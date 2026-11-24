"use client"

import { useEffect, useState, type FormEvent } from "react"
import { Copy, KeyRound, Plus } from "lucide-react"
import { ApiError, createApiToken, fetchApiTokens, revokeApiToken, type ApiTokenExpirationDays, type PersonalApiToken } from "@/lib/api"
import { getHubUrl } from "@/lib/hub-url"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "Something went wrong. Please try again."
}

function formatDate(value: string | null): string {
  if (!value) return "Never"
  return new Date(value).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" })
}

// hubPublicUrl is the hub's configured public URL. Without it, fall back to the
// build-time hub URL or this origin, which is only right when the hub serves the UI.
export function ApiTokensSettings({ hubPublicUrl = "" }: { hubPublicUrl?: string }) {
  const [tokens, setTokens] = useState<PersonalApiToken[] | null>(null)
  const [error, setError] = useState("")
  const [githubRequired, setGithubRequired] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [expiration, setExpiration] = useState("30")
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState("")
  const [revealedToken, setRevealedToken] = useState<string | null>(null)
  const [command, setCommand] = useState("")
  const [copyMessage, setCopyMessage] = useState("")
  const [revokeTarget, setRevokeTarget] = useState<PersonalApiToken | null>(null)
  const [revoking, setRevoking] = useState(false)
  const [revokeError, setRevokeError] = useState("")

  useEffect(() => {
    let cancelled = false
    fetchApiTokens().then((data) => {
      if (!cancelled) setTokens(data.tokens)
    }).catch((err) => {
      if (cancelled) return
      setGithubRequired(err instanceof ApiError && err.status === 403 && /GitHub sign-in/i.test(err.message))
      setError(errorMessage(err))
    })
    return () => { cancelled = true }
  }, [])

  async function handleCreate(event: FormEvent) {
    event.preventDefault()
    if (creating || !name.trim()) return
    setCreating(true)
    setCreateError("")
    try {
      const hub = new URL(hubPublicUrl || getHubUrl() || window.location.origin, window.location.origin)
      const mcpUrl = `${hub.origin}${hub.pathname.replace(/\/+$/, "")}/mcp`
      const { token, ...metadata } = await createApiToken(name.trim(), expiration === "never" ? null : Number(expiration) as ApiTokenExpirationDays)
      setTokens((current) => [metadata, ...(current ?? [])])
      setCommand(`claude mcp add --transport http factory ${mcpUrl} --header "Authorization: Bearer ${token}"`)
      setCopyMessage("")
      setRevealedToken(token)
      setCreateOpen(false)
      setName("")
      setExpiration("30")
    } catch (err) {
      setCreateError(errorMessage(err))
    } finally {
      setCreating(false)
    }
  }

  async function handleRevoke() {
    if (!revokeTarget || revoking) return
    setRevoking(true)
    setRevokeError("")
    try {
      await revokeApiToken(revokeTarget.id)
      setTokens((current) => current?.map((token) => token.id === revokeTarget.id
        ? { ...token, status: "revoked", revoked_at: new Date().toISOString() } : token) ?? [])
      setRevokeTarget(null)
    } catch (err) {
      setRevokeError(errorMessage(err))
    } finally {
      setRevoking(false)
    }
  }

  async function copy(value: string, label: string) {
    try {
      await navigator.clipboard.writeText(value)
      setCopyMessage(`${label} copied.`)
    } catch {
      setCopyMessage("Could not copy. Select and copy the text manually.")
    }
  }

  function closeReveal() {
    setRevealedToken(null)
    setCommand("")
    setCopyMessage("")
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="max-w-xl">
          <h2 className="text-base font-semibold mb-1">API Tokens</h2>
          <p className="text-sm text-muted-foreground">Personal tokens for the Factory MCP server. Only you can see and manage your tokens; access ends if you stop being an admin.</p>
        </div>
        {tokens !== null && <Button size="sm" onClick={() => { setCreateError(""); setCreateOpen(true) }}><Plus className="size-4" />Create token</Button>}
      </div>

      {githubRequired ? (
        <div className="rounded-lg border p-6 space-y-2">
          <KeyRound className="size-5 text-muted-foreground" />
          <h3 className="text-sm font-medium">API tokens require GitHub sign-in</h3>
          <p className="text-sm text-muted-foreground">You are signed in with a password or hub token. Sign out and sign in with GitHub to create and manage personal API tokens.</p>
        </div>
      ) : error ? <p role="alert" className="text-sm text-destructive">{error}</p>
        : tokens === null ? <p className="text-sm text-muted-foreground animate-pulse">Loading tokens…</p>
        : tokens.length === 0 ? (
          <div className="rounded-lg border p-8 text-center space-y-2">
            <KeyRound className="size-6 mx-auto text-muted-foreground" />
            <h3 className="text-sm font-medium">No API tokens yet</h3>
            <p className="text-sm text-muted-foreground">Create a personal token to connect your MCP client to Factory.</p>
          </div>
        ) : (
          <div className="rounded-lg border overflow-hidden">
            <Table>
              <TableHeader><TableRow>
                {["Name", "Token", "Created", "Last used", "Expires", "Status"].map((label) => <TableHead key={label}>{label}</TableHead>)}
                <TableHead><span className="sr-only">Actions</span></TableHead>
              </TableRow></TableHeader>
              <TableBody>{tokens.map((token) => (
                <TableRow key={token.id} className={token.status !== "active" ? "text-muted-foreground" : undefined}>
                  <TableCell className="font-medium max-w-48 truncate" title={token.name}>{token.name}</TableCell>
                  <TableCell className="font-mono">{token.prefix}…</TableCell>
                  <TableCell>{formatDate(token.created_at)}</TableCell>
                  <TableCell>{formatDate(token.last_used_at)}</TableCell>
                  <TableCell>{formatDate(token.expires_at)}</TableCell>
                  <TableCell><Badge variant={token.status === "active" ? "secondary" : "outline"} className={token.status !== "active" ? "text-muted-foreground" : undefined}>{token.status}</Badge></TableCell>
                  <TableCell>{token.status === "active" && <Button variant="ghost" size="sm" onClick={() => { setRevokeError(""); setRevokeTarget(token) }}>Revoke</Button>}</TableCell>
                </TableRow>
              ))}</TableBody>
            </Table>
          </div>
        )}

      <Dialog open={createOpen} onOpenChange={(open) => { if (!creating) setCreateOpen(open) }}>
        <DialogContent>
          <DialogHeader><DialogTitle>Create API token</DialogTitle><DialogDescription>Give your token a name that identifies where you will use it.</DialogDescription></DialogHeader>
          <form onSubmit={handleCreate} className="space-y-4">
            <div className="space-y-2"><Label htmlFor="api-token-name">Name</Label><Input id="api-token-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Claude Code on my laptop" required disabled={creating} autoComplete="off" /></div>
            <div className="space-y-2"><Label htmlFor="api-token-expiration">Expiration</Label>
              <Select value={expiration} onValueChange={setExpiration} disabled={creating}>
                <SelectTrigger id="api-token-expiration"><SelectValue /></SelectTrigger>
                <SelectContent>{[7, 30, 90, 180].map((days) => <SelectItem key={days} value={String(days)}>{days} days</SelectItem>)}<SelectItem value="never">Never</SelectItem></SelectContent>
              </Select>
            </div>
            {createError && <p role="alert" className="text-sm text-destructive">{createError}</p>}
            <DialogFooter><Button type="button" variant="outline" disabled={creating} onClick={() => setCreateOpen(false)}>Cancel</Button><Button type="submit" disabled={creating || !name.trim()}>{creating ? "Creating…" : "Create token"}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={revealedToken !== null} onOpenChange={(open) => { if (!open) closeReveal() }}>
        <DialogContent onInteractOutside={(event) => event.preventDefault()} onEscapeKeyDown={(event) => event.preventDefault()}>
          <DialogHeader><DialogTitle>Save your API token</DialogTitle><DialogDescription>Copy this token now and store it securely. It won’t be shown again.</DialogDescription></DialogHeader>
          <div className="space-y-2"><Label htmlFor="created-api-token">Token</Label><Input id="created-api-token" readOnly value={revealedToken ?? ""} className="font-mono text-xs" onFocus={(event) => event.target.select()} /><Button variant="outline" size="sm" onClick={() => copy(revealedToken ?? "", "Token")}><Copy className="size-4" />Copy token</Button></div>
          <div className="space-y-2"><p className="text-sm font-medium">Connect Claude Code</p><pre className="rounded-md bg-muted p-3 text-xs font-mono whitespace-pre-wrap break-all select-all">{command}</pre><Button variant="outline" size="sm" onClick={() => copy(command, "Command")}><Copy className="size-4" />Copy command</Button></div>
          <p role="status" className="text-sm text-muted-foreground">{copyMessage}</p>
          <DialogFooter><Button onClick={closeReveal}>Done</Button></DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={revokeTarget !== null} onOpenChange={(open) => { if (!open && !revoking) setRevokeTarget(null) }}>
        <DialogContent>
          <DialogHeader><DialogTitle>Revoke API token?</DialogTitle><DialogDescription>Revoking “{revokeTarget?.name}” immediately prevents further authentication with it. This cannot be undone.</DialogDescription></DialogHeader>
          {revokeError && <p role="alert" className="text-sm text-destructive">{revokeError}</p>}
          <DialogFooter><Button variant="outline" disabled={revoking} onClick={() => setRevokeTarget(null)}>Cancel</Button><Button variant="destructive" disabled={revoking} onClick={handleRevoke}>{revoking ? "Revoking…" : "Revoke token"}</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
