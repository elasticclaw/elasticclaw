"use client"

import { useEffect, useRef } from "react"
import "@xterm/xterm/css/xterm.css"

interface TerminalProps {
  clawId: string
  wsUrl: string
  className?: string
}

/**
 * xterm.js needs concrete color strings, so design tokens are resolved from the
 * document at mount time instead of being handed over as `var(--…)`.
 */
function readToken(name: string, fallback: string): string {
  if (typeof window === "undefined") return fallback
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return value || fallback
}

/**
 * XTerminal — browser-only SSH terminal component.
 * Dynamically imports xterm.js to avoid SSR issues.
 */
export function XTerminal({ clawId, wsUrl, className }: TerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const stateRef = useRef<{
    ws: WebSocket | null
    term: import("@xterm/xterm").Terminal | null
    fitAddon: import("@xterm/addon-fit").FitAddon | null
    resizeObserver: ResizeObserver | null
  }>({ ws: null, term: null, fitAddon: null, resizeObserver: null })

  useEffect(() => {
    if (!containerRef.current) return
    const el = containerRef.current
    let mounted = true

    async function init() {
      const { Terminal } = await import("@xterm/xterm")
      const { FitAddon } = await import("@xterm/addon-fit")
      if (!mounted || !containerRef.current) return

      const surface = readToken("--ds-surface", "#1e1e21")
      const text = readToken("--ds-text", "#f2f2f4")

      const term = new Terminal({
        cursorBlink: true,
        fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", monospace',
        fontSize: 13,
        theme: {
          background: surface,
          foreground: text,
          cursor: "#ff563c",
          cursorAccent: surface,
          selectionBackground: "rgba(255, 86, 60, 0.32)",
          selectionForeground: text,
          // ANSI 16 tuned for the Modernist dark surface: the red family follows
          // the accent, the other hues stay distinguishable as semantic ANSI slots.
          black: "#2b2b30",
          red: "#ff563c",
          green: "#2fb37a",
          yellow: "#d9a13a",
          blue: "#6f9dea",
          magenta: "#c98ad6",
          cyan: "#4fbfc4",
          white: "#d5d5d8",
          brightBlack: "#7b7b80",
          brightRed: "#ff8a76",
          brightGreen: "#57d19c",
          brightYellow: "#f0bf5f",
          brightBlue: "#93b8f4",
          brightMagenta: "#e0aeea",
          brightCyan: "#7ad9dd",
          brightWhite: "#f7f7f8",
        },
      })

      const fitAddon = new FitAddon()
      term.loadAddon(fitAddon)
      term.open(containerRef.current)
      fitAddon.fit()

      stateRef.current.term = term
      stateRef.current.fitAddon = fitAddon

      // Connect WebSocket
      const ws = new WebSocket(wsUrl)
      stateRef.current.ws = ws

      ws.onopen = () => {
        term.writeln("\r\n\x1b[32mConnected to SSH terminal\x1b[0m\r\n")
        fitAddon.fit()
        // Send initial size
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }))
      }

      ws.onmessage = (e) => {
        term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data))
      }

      ws.onerror = () => {
        term.writeln("\r\n\x1b[31mWebSocket error — connection failed\x1b[0m\r\n")
      }

      ws.onclose = () => {
        term.writeln("\r\n\x1b[33mConnection closed\x1b[0m\r\n")
      }

      term.onData((data) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(data)
        }
      })

      term.onResize(({ cols, rows }) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "resize", cols, rows }))
        }
      })

      // Observe container resize
      const resizeObserver = new ResizeObserver(() => {
        if (mounted) fitAddon.fit()
      })
      resizeObserver.observe(el)
      stateRef.current.resizeObserver = resizeObserver
    }

    init().catch(console.error)

    return () => {
      mounted = false
      const { ws, term, fitAddon, resizeObserver } = stateRef.current
      resizeObserver?.disconnect()
      ws?.close()
      term?.dispose()
      stateRef.current = { ws: null, term: null, fitAddon: null, resizeObserver: null }
    }
  }, [wsUrl]) // reconnect if wsUrl changes

  return (
    <div
      ref={containerRef}
      className={className}
      style={{ background: "var(--ds-surface)" }}
    />
  )
}
