import { useEffect, useRef } from "react"

type EscapeLayer = { id: symbol; onClose: () => void }

const escapeLayers: EscapeLayer[] = []

function handleEscape(event: KeyboardEvent) {
  if (event.key !== "Escape") return
  escapeLayers.at(-1)?.onClose()
}

export function useEscapeToClose(onClose: () => void, active = true) {
  const layerId = useRef(Symbol("escape-layer"))
  const onCloseRef = useRef(onClose)

  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  useEffect(() => {
    if (!active) return
    const layer: EscapeLayer = { id: layerId.current, onClose: () => onCloseRef.current() }
    escapeLayers.push(layer)
    if (escapeLayers.length === 1) document.addEventListener("keydown", handleEscape)
    return () => {
      const index = escapeLayers.findIndex(({ id }) => id === layer.id)
      if (index !== -1) escapeLayers.splice(index, 1)
      if (escapeLayers.length === 0) document.removeEventListener("keydown", handleEscape)
    }
  }, [active])
}
