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
 *
 * `unpin` is for content that grows *below* its anchor (a clamped message
 * expanding): it releases the bottom pin first, so the auto-scroll does not
 * drag the reader to the end of what they just opened.
 */
export type ToggleAnchor = (el: HTMLElement, options?: { unpin?: boolean }) => void

export const ToggleAnchorContext = createContext<ToggleAnchor>(() => {})

export function useToggleAnchor(): ToggleAnchor {
  return useContext(ToggleAnchorContext)
}
