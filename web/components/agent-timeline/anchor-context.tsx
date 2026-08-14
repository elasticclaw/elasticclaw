"use client"

import { createContext, useContext } from "react"

/**
 * Expanding/collapsing timeline content above the viewport shifts everything
 * below it. Components call `anchor(el)` with a persistent element (the row
 * or header being toggled) right before flipping their expanded state; the
 * timeline owner measures the element, lets React flush the discrete-event
 * update synchronously, and compensates scrollTop in a microtask so the
 * reading position does not move. The default is a no-op (board cards pin to
 * bottom and do not need it).
 */
export const ToggleAnchorContext = createContext<(el: HTMLElement) => void>(() => {})

export function useToggleAnchor(): (el: HTMLElement) => void {
  return useContext(ToggleAnchorContext)
}
