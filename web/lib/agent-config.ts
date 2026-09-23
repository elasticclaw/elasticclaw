export interface AgentConfig {
  default_model?: string
  llm_key?: string
  subagents?: {
    model?: string
    llm_key?: string
    max_concurrent?: number
  }
}

export interface AgentOptions {
  credentials: { name: string; provider: string; default_model: string; available: boolean }[]
  default_model: string
}

export function validateAgentConfig(config: AgentConfig): string | null {
  const count = config.subagents?.max_concurrent
  if (count !== undefined && (!Number.isInteger(count) || count < 1 || count > 32)) {
    return "Concurrent subagents must be a whole number between 1 and 32."
  }
  return null
}
