"use client"

import { useEffect, useState } from "react"

// One shared 1s interval for every subscriber (moved from conversation-view).
// Live elapsed/age labels across cards, step rows, and the Now strip all tick
// together instead of each mounting its own timer.
let sharedNow = Date.now()
// Browser timer handle (window.setInterval returns a number, not a Node Timeout)
let sharedTimer: number | null = null
const listeners = new Set<() => void>()
let sharedMinuteNow = Date.now()
let sharedMinuteTimer: number | null = null
const minuteListeners = new Set<() => void>()

/**
 * Returns Date.now() refreshed once per second while `active` is true.
 * Inactive subscribers keep the mount-time value and no timer runs when no
 * subscriber is active. SSR-safe: the initial value renders once and any
 * drift is corrected client-side (render times with suppressHydrationWarning).
 */
export function useNowTick(active: boolean): number {
  const [now, setNow] = useState(sharedNow)
  useEffect(() => {
    if (!active) return
    const listener = () => setNow(sharedNow)
    listeners.add(listener)
    if (!sharedTimer) {
      sharedTimer = window.setInterval(() => {
        sharedNow = Date.now()
        listeners.forEach((notify) => notify())
      }, 1_000)
    }
    listener()
    return () => {
      listeners.delete(listener)
      if (listeners.size === 0 && sharedTimer) {
        window.clearInterval(sharedTimer)
        sharedTimer = null
      }
    }
  }, [active])
  return now
}

/** Returns a clock refreshed once per minute for age labels. */
export function useNowMinuteTick(active: boolean): number {
  const [now, setNow] = useState(sharedMinuteNow)
  useEffect(() => {
    if (!active) return
    const listener = () => setNow(sharedMinuteNow)
    minuteListeners.add(listener)
    if (!sharedMinuteTimer) {
      sharedMinuteNow = Date.now()
      sharedMinuteTimer = window.setInterval(() => {
        sharedMinuteNow = Date.now()
        minuteListeners.forEach((notify) => notify())
      }, 60_000)
    }
    listener()
    return () => {
      minuteListeners.delete(listener)
      if (minuteListeners.size === 0 && sharedMinuteTimer) {
        window.clearInterval(sharedMinuteTimer)
        sharedMinuteTimer = null
      }
    }
  }, [active])
  return now
}
