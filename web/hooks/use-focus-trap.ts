import { useEffect } from "react"
import type { RefObject } from "react"

const focusableSelector = '[href], button, [tabindex]:not([tabindex="-1"]), input, select, textarea'

export function useFocusTrap(containerRef: RefObject<HTMLElement | null>, active: boolean, dependency?: unknown) {
  useEffect(() => {
    const container = containerRef.current
    if (!active || !container) return

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Tab") return
      const focusable = [...container.querySelectorAll<HTMLElement>(focusableSelector)].filter(
        (element) => !element.hasAttribute("disabled") && element.tabIndex >= 0,
      )
      if (focusable.length === 0) {
        event.preventDefault()
        container.focus()
        return
      }

      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }

    container.addEventListener("keydown", handleKeyDown)
    return () => container.removeEventListener("keydown", handleKeyDown)
  }, [active, containerRef, dependency])
}
