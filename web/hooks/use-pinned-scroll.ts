"use client"

import { useCallback, useEffect, useLayoutEffect, useRef } from "react"

/**
 * Writing `scrollTop` fires a real `scroll` event, so a scroll handler that
 * derives state (pinned-to-bottom, load-older triggers) from the position
 * would recompute it from a mid-animation value. Callers invoke `mark()`
 * right before any programmatic `scrollTop` write and early-return from
 * their scroll handlers while `isProgrammatic.current` is true; the flag
 * clears itself on the next animation frame, after the browser has
 * dispatched the resulting scroll event.
 */
export function useProgrammaticScrollFlag(): {
  isProgrammaticRef: React.RefObject<boolean>
  mark: () => void
} {
  const isProgrammaticRef = useRef(false)
  const mark = useCallback(() => {
    isProgrammaticRef.current = true
    requestAnimationFrame(() => {
      isProgrammaticRef.current = false
    })
  }, [])
  return { isProgrammaticRef, mark }
}

interface PinnedAutoScrollOptions {
  scrollRef: React.RefObject<HTMLElement | null>
  /** Element wrapping the scroller's content — its resize means content settled. */
  contentRef: React.RefObject<HTMLElement | null>
  /** True while the user is at (or intends to be at) the bottom. */
  pinnedRef: React.RefObject<boolean>
  markProgrammaticScroll: () => void
  /** Value whose change should immediately re-pin (typically the messages array). */
  bottomAnchor: unknown
}

/**
 * Keeps a transcript scroller glued to its bottom while `pinnedRef` is true,
 * and never touches it otherwise. Two mechanisms cover the two ways content
 * grows: a layout effect pins synchronously when React commits new rows, and
 * a ResizeObserver re-pins when already-committed content settles its height
 * later (markdown, images, tables, streaming text) — which is what timer
 * cascades used to approximate, minus the stale jumps that landed after the
 * user had scrolled away.
 */
export function usePinnedAutoScroll({
  scrollRef,
  contentRef,
  pinnedRef,
  markProgrammaticScroll,
  bottomAnchor,
}: PinnedAutoScrollOptions) {
  useLayoutEffect(() => {
    if (!pinnedRef.current) return
    const el = scrollRef.current
    if (!el) return
    markProgrammaticScroll()
    el.scrollTop = el.scrollHeight
  }, [bottomAnchor, pinnedRef, scrollRef, markProgrammaticScroll])

  useEffect(() => {
    const el = scrollRef.current
    const content = contentRef.current
    if (!el || !content) return
    const observer = new ResizeObserver(() => {
      if (!pinnedRef.current) return
      markProgrammaticScroll()
      el.scrollTop = el.scrollHeight
    })
    observer.observe(content)
    return () => observer.disconnect()
  }, [scrollRef, contentRef, pinnedRef, markProgrammaticScroll])
}
