"use client"

import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"
import type { SettingsData } from "./types"

export default function AuthenticationSection({ settings, onSave, saving }: { settings: SettingsData; onSave: (p: object) => void; saving: boolean }) {
  const ghOAuth = settings.auth?.githubOAuth
  const ghAccess = settings.auth?.access
  const ghAllowedUsers = ghOAuth?.allowedUsers || []
  const ghAllowedOrgs = ghOAuth?.allowedOrgs || []
  const ghAllowedTeams = ghOAuth?.allowedTeams || []
  const ghAdmins = ghAccess?.admins || []
  const ghViewRequiresTags = ghAccess?.viewRequiresTags || []
  const ghInteractRequiresTags = ghAccess?.interactRequiresTags || []

  // Password card state
  const [newPw, setNewPw] = useState('')
  const [pwConfirm, setPwConfirm] = useState('')
  const [pwErr, setPwErr] = useState('')

  // GitHub OAuth config state
  const [showGhForm, setShowGhForm] = useState(false)
  const [clientId, setClientId] = useState(ghOAuth?.clientId || '')
  const [clientSecret, setClientSecret] = useState('')
  const [allowedUsers, setAllowedUsers] = useState(ghAllowedUsers.join(', '))
  const [allowedOrgs, setAllowedOrgs] = useState(ghAllowedOrgs.join(', '))
  const [allowedTeams, setAllowedTeams] = useState(ghAllowedTeams.join(', '))
  const [admins, setAdmins] = useState(ghAdmins.join(', '))
  const [viewTags, setViewTags] = useState(ghViewRequiresTags.join(', '))
  const [interactTags, setInteractTags] = useState(ghInteractRequiresTags.join(', '))
  const [ghErr, setGhErr] = useState('')

  function handlePasswordSave() {
    setPwErr('')
    if (newPw.length < 8) { setPwErr('Password must be at least 8 characters'); return }
    if (newPw !== pwConfirm) { setPwErr('Passwords do not match'); return }
    onSave({ uiPassword: newPw })
    setNewPw(''); setPwConfirm('')
  }

  function splitList(s: string) {
    return s.split(/[,\n]+/).map((t: string) => t.trim()).filter(Boolean)
  }

  function handleGitHubSave() {
    setGhErr('')
    if (!clientId.trim()) { setGhErr('Client ID is required'); return }
    if (!ghOAuth && !clientSecret.trim()) { setGhErr('Client Secret is required for initial setup'); return }
    onSave({
      auth: {
        githubOAuth: {
          clientId: clientId.trim(),
          ...(clientSecret ? { clientSecret: clientSecret.trim() } : {}),
          allowedUsers: splitList(allowedUsers),
          allowedOrgs: splitList(allowedOrgs),
          allowedTeams: splitList(allowedTeams),
        },
        access: {
          admins: splitList(admins),
          viewRequiresTags: splitList(viewTags),
          interactRequiresTags: splitList(interactTags),
        },
      }
    })
    setClientSecret('')
    setShowGhForm(false)
  }

  function handleGitHubRemove() {
    if (!window.confirm('Disable GitHub OAuth? Users will only be able to log in with the password.')) return
    onSave({ auth: { removeGithubOAuth: true } })
  }

  function handleGitHubEdit() {
    setClientId(ghOAuth?.clientId || '')
    setClientSecret('')
    setAllowedUsers(ghAllowedUsers.join(', '))
    setAllowedOrgs(ghAllowedOrgs.join(', '))
    setAllowedTeams(ghAllowedTeams.join(', '))
    setAdmins(ghAdmins.join(', '))
    setViewTags(ghViewRequiresTags.join(', '))
    setInteractTags(ghInteractRequiresTags.join(', '))
    setShowGhForm(true)
  }

  const callbackUrl = typeof window !== 'undefined'
    ? window.location.origin + '/login'
    : 'https://your-hub/login'

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-0.5">Authentication</h2>
        <p className="text-sm text-muted-foreground">Both methods can be active simultaneously. GitHub OAuth users and password users both have full access unless tag-based restrictions are configured.</p>
      </div>

      {/* Password card */}
      <div className="border border-border rounded-lg p-5 space-y-3 max-w-lg">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium">Password Login</h3>
            <p className="text-xs text-muted-foreground mt-0.5">
              {settings.auth?.disablePasswordAuth ? 'Disabled — GitHub OAuth only.' : 'Enabled. Used by the hub token and UI password.'}
            </p>
          </div>
          {settings.auth?.disablePasswordAuth
            ? <span className="text-xs bg-muted text-muted-foreground px-2 py-0.5 rounded font-medium">Disabled</span>
            : <span className="text-xs bg-green-500/20 text-green-400 px-2 py-0.5 rounded font-medium">Active</span>
          }
        </div>

        {/* Disable/enable toggle — only show when GitHub OAuth is configured */}
        {ghOAuth && (
          <div className="flex items-center justify-between border border-border rounded-md px-3 py-2">
            <div>
              <p className="text-xs font-medium">Require GitHub OAuth</p>
              <p className="text-xs text-muted-foreground mt-0.5">Disable password login entirely. Make sure you can log in via GitHub first.</p>
            </div>
            <button
              onClick={() => onSave({ auth: { disablePasswordAuth: !settings.auth?.disablePasswordAuth } })}
              disabled={saving}
              className={cn(
                'relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors',
                settings.auth?.disablePasswordAuth ? 'bg-primary' : 'bg-muted'
              )}
            >
              <span className={cn(
                'pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-lg transform transition-transform',
                settings.auth?.disablePasswordAuth ? 'translate-x-4' : 'translate-x-0'
              )} />
            </button>
          </div>
        )}

        {!settings.auth?.disablePasswordAuth && (
          <div className="border-t border-border pt-3 space-y-3">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">New Password</label>
              <Input type="password" value={newPw} onChange={e => setNewPw(e.target.value)} className="h-8 text-sm" placeholder="Min 8 characters" />
            </div>
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Confirm Password</label>
              <Input type="password" value={pwConfirm} onChange={e => setPwConfirm(e.target.value)} className="h-8 text-sm" placeholder="Repeat password" />
            </div>
            {pwErr && <p className="text-xs text-red-500">{pwErr}</p>}
            <Button size="sm" disabled={saving || !newPw || !pwConfirm} onClick={handlePasswordSave}>
              Change Password
            </Button>
          </div>
        )}
      </div>

      {/* GitHub OAuth card */}
      <div className="border border-border rounded-lg p-5 space-y-3 max-w-lg">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium">GitHub OAuth</h3>
            <p className="text-xs text-muted-foreground mt-0.5">Let users log in with their GitHub account.</p>
          </div>
          {ghOAuth
            ? <span className="text-xs bg-green-500/20 text-green-400 px-2 py-0.5 rounded font-medium">Active</span>
            : <span className="text-xs bg-muted text-muted-foreground px-2 py-0.5 rounded font-medium">Inactive</span>
          }
        </div>

        {ghOAuth && !showGhForm && (
          <div className="border-t border-border pt-3 space-y-2 text-xs text-muted-foreground">
            <div className="flex gap-2"><span className="font-medium text-foreground w-28">Client ID</span><span className="font-mono">{ghOAuth.clientId}</span></div>
            <div className="flex gap-2"><span className="font-medium text-foreground w-28">Client Secret</span><span>{ghOAuth.clientSecretSet ? '••••••••' : 'not set'}</span></div>
            {ghAdmins.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Admins</span><span>{ghAdmins.join(', ')}</span></div>}
            {ghAllowedUsers.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed users</span><span>{ghAllowedUsers.join(', ')}</span></div>}
            {ghAllowedOrgs.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed orgs</span><span>{ghAllowedOrgs.join(', ')}</span></div>}
            {ghAllowedTeams.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Allowed teams</span><span>{ghAllowedTeams.join(', ')}</span></div>}
            {ghViewRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">View tag filter</span><span className="font-mono">{ghViewRequiresTags.join(', ')}</span></div>}
            {ghInteractRequiresTags.length > 0 && <div className="flex gap-2"><span className="font-medium text-foreground w-28">Interact tag filter</span><span className="font-mono">{ghInteractRequiresTags.join(', ')}</span></div>}
            <div className="flex gap-2 pt-1">
              <Button size="sm" variant="outline" onClick={handleGitHubEdit}>Edit</Button>
              <Button size="sm" variant="outline" onClick={handleGitHubRemove} className="text-destructive hover:text-destructive">Disable</Button>
            </div>
          </div>
        )}

        {(!ghOAuth || showGhForm) && (
          <div className="border-t border-border pt-3 space-y-4">
            {!ghOAuth && (
              <p className="text-xs text-muted-foreground">
                Create a <a href="https://github.com/settings/developers" target="_blank" rel="noopener noreferrer" className="underline">GitHub OAuth App</a> and set the callback URL to{' '}
                <code className="bg-muted px-1 rounded">{callbackUrl}</code>.
              </p>
            )}

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">App Credentials</h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Client ID</label>
                <Input value={clientId} onChange={e => setClientId(e.target.value)} className="h-8 text-sm font-mono" placeholder="Ov23li..." />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">
                  Client Secret{ghOAuth?.clientSecretSet && <span className="ml-1 text-green-500">(set)</span>}
                </label>
                <Input
                  type="password"
                  value={clientSecret}
                  onChange={e => setClientSecret(e.target.value)}
                  className="h-8 text-sm font-mono"
                  placeholder={ghOAuth?.clientSecretSet ? 'Leave blank to keep existing' : 'Paste secret...'}
                />
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Allowlist <span className="normal-case font-normal">(leave blank = any GitHub user)</span></h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Usernames</label>
                <Input value={allowedUsers} onChange={e => setAllowedUsers(e.target.value)} className="h-8 text-sm" placeholder="alice, bob" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Orgs</label>
                <Input value={allowedOrgs} onChange={e => setAllowedOrgs(e.target.value)} className="h-8 text-sm" placeholder="my-org" />
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Teams</label>
                <Input value={allowedTeams} onChange={e => setAllowedTeams(e.target.value)} className="h-8 text-sm" placeholder="my-org/my-team" />
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Admins</h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">GitHub Usernames</label>
                <Input value={admins} onChange={e => setAdmins(e.target.value)} className="h-8 text-sm" placeholder="alice, bob" />
                <p className="text-xs text-muted-foreground mt-1">Comma-separated. Admins can access Settings and bypass all tag restrictions.</p>
              </div>
            </div>

            <div className="space-y-3">
              <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Tag-Based Access Control <span className="normal-case font-normal">(optional)</span></h4>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">View requires tag</label>
                <Input value={viewTags} onChange={e => setViewTags(e.target.value)} className="h-8 text-sm font-mono" placeholder="user, team=frontend" />
                <p className="text-xs text-muted-foreground mt-1">Agent must have a tag like <code className="bg-muted px-1 rounded">user=alice</code> for that user to see it.</p>
              </div>
              <div>
                <label className="text-xs text-muted-foreground mb-1 block">Interact requires tag</label>
                <Input value={interactTags} onChange={e => setInteractTags(e.target.value)} className="h-8 text-sm font-mono" placeholder="user, team=frontend" />
              </div>
            </div>

            {ghErr && <p className="text-xs text-red-500">{ghErr}</p>}

            <div className="flex gap-2">
              <Button size="sm" disabled={saving} onClick={handleGitHubSave}>
                {ghOAuth ? 'Save Changes' : 'Enable GitHub OAuth'}
              </Button>
              {showGhForm && (
                <Button size="sm" variant="outline" onClick={() => { setShowGhForm(false); setGhErr('') }}>Cancel</Button>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
