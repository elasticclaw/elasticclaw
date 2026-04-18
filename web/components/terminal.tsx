"use client"

import { useEffect, useRef } from "react"
import "@xterm/xterm/css/xterm.css"

interface TerminalProps {
  clawId: string
  wsUrl: string
  className?: string
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

      const term = new Terminal({
        cursorBlink: true,
        fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", monospace',
        fontSize: 13,
        theme: {
          background: "#1e1e1e",
          foreground: "#d4d4d4",
          cursor: "#d4d4d4",
          selectionBackground: "#264f78",
          black: "#1e1e1e",
          red: "#f44747",
          green: "#4ec9b0",
          yellow: "#dcdcaa",
          blue: "#569cd6",
          magenta: "#c586c0",
          cyan: "#9cdcfe",
          white: "#d4d4d4",
          brightBlack: "#808080",
          brightRed: "#f44747",
          brightGreen: "#4ec9b0",
          brightYellow: "#dcdcaa",
          brightBlue: "#569cd6",
          brightMagenta: "#c586c0",
          brightCyan: "#9cdcfe",
          brightWhite: "#ffffff",
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
      style={{ background: "#1e1e1e" }}
    />
  )
}
