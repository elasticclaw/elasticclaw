import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/**
 * Exhaustiveness helper: call in the `default`/fallthrough branch of a switch
 * over a union type. If a new union member appears (e.g. a new claw status in
 * the generated API types), the compiler flags the call site instead of the
 * value silently falling through.
 */
export function assertNever(value: never): never {
  throw new Error(`Unhandled value: ${JSON.stringify(value)}`)
}
