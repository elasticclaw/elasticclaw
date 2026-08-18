const PERSON_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)'] as const

const FIXTURE_PERSON_COLORS = {
  marc: 'var(--chart-1)',
  priya: 'var(--chart-3)',
  dana: 'var(--chart-2)',
  sofia: 'var(--chart-5)',
} as const

export function personColor(login: string): string {
  const fixtureColor = FIXTURE_PERSON_COLORS[login as keyof typeof FIXTURE_PERSON_COLORS]
  if (fixtureColor) return fixtureColor

  let hash = 0
  for (let index = 0; index < login.length; index += 1) hash = (hash * 31 + login.charCodeAt(index)) | 0
  return PERSON_COLORS[Math.abs(hash) % PERSON_COLORS.length]
}
