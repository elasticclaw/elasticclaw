// Shared view types for the settings sections. These mirror the JSON returned
// by GET /api/settings and are consumed by every section component.

export interface LLMKeyView {
  name: string
  provider: string
  keySet: boolean
  default: boolean
  defaultModel?: string
  authProfile?: string
}

export interface ModelAuthProfileView {
  name: string
  provider: string
  mode: string
  authenticated: boolean
  updatedAt?: string
}

export interface LLMModelOption {
  id: string
  name: string
}

export interface GitHubAppPermission {
  name: string
  granted: string
  needed: string
  ok: boolean
}

export interface GitHubAppView {
  appId: number
  url?: string
  keySet: boolean
  permissions?: GitHubAppPermission[]
  permCheckOk?: boolean
  permCheckError?: string
}

export interface WorkspaceGitHubAppView {
  name: string
  appId: number
  url?: string
  installation?: string
  installations?: string[]
  private_key_set?: boolean
  privateKeySet?: boolean
}

export interface SettingsData {
  llmKeys: LLMKeyView[]
  modelOptions?: Record<string, LLMModelOption[]>
  modelAuthProfiles?: ModelAuthProfileView[]
  providers: Record<string, {
    type: string
    enabled: boolean
    apiUrl?: string
    apiKeySet?: boolean
    defaultSnapshot?: string
    tokenSet?: boolean
    defaultTtl?: string
    defaultInstanceType?: string
    defaultCpu?: number
    defaultMemory?: string
    defaultDisk?: string
    sshKeySet?: boolean
    sshPublicKey?: string
    image?: string
    network?: string
    awsRegion?: string
    awsProfile?: string
    imageIdentifier?: string
    imageVersion?: string
    executionRoleArn?: string
    ingressNetworkConnectors?: string[]
    egressNetworkConnectors?: string[]
    idleMaxDurationSeconds?: number
    suspendedDurationSeconds?: number
    autoResume?: boolean
    maximumDurationSeconds?: number
    bridgePort?: number
    authTokenExpirationMinutes?: number
  }>
  github: GitHubAppView[]
  sshPublicKeys: string[]
  integrations?: {
    linear?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    shortcut?: Array<{ workspace: string; tokenSet: boolean }>
    githubIssues?: Array<{ workspace: string; tokenSet: boolean; webhookSecretSet: boolean }>
    jira?: Array<{ workspace: string; baseUrl?: string; username?: string; tokenSet: boolean; webhookSecretSet: boolean }>
  }
  secrets?: string[]
  mcpServers?: Array<{
    name: string
    source: string
    package?: string
    image?: string
    url?: string
    enabled: boolean
    config?: Record<string, string>
    secrets?: string[]
    command?: string[]
  }>
  auth?: {
    githubOAuth?: {
      clientId: string
      clientSecretSet: boolean
      allowedUsers: string[]
      allowedOrgs: string[]
      allowedTeams: string[]
    }
    access?: {
      admins: string[]
      viewRequiresTags: string[]
      interactRequiresTags: string[]
    }
    disablePasswordAuth?: boolean
  }
  concurrencyGroups?: ConcurrencyGroup[]
  maxConcurrentClaws?: number
}

export interface ConcurrencyGroup {
  name: string
  limit: number
}
