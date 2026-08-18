const PERSON_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)'] as const

export function personColor(login: string): string {
  let hash = 0
  for (let index = 0; index < login.length; index += 1) hash = (hash * 31 + login.charCodeAt(index)) | 0
  return PERSON_COLORS[Math.abs(hash) % PERSON_COLORS.length]
}
