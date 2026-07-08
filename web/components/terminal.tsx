"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { useWebSocket } from "@/hooks/use-websocket"
import "@xterm/xterm/css/xterm.css"

interface TerminalProps {
  clawId: string
  wsUrl: string
  className?: string
}

/**
 * XTerminal — browser-only SSH terminal component.
 * Dynamically imports xterm.js to avoid SSR issues. The WebSocket lifecycle
 * is owned by the shared useWebSocket hook (no auto-reconnect: an SSH session
 * cannot be resumed transparently, so a dropped connection stays closed).
 */
export function XTerminal({ clawId, wsUrl, className }: TerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<import("@xterm/xterm").Terminal | null>(null)
  const fitAddonRef = useRef<import("@xterm/addon-fit").FitAddon | null>(null)
  const [termReady, setTermReady] = useState(false)

  const getUrl = useCallback(() => wsUrl, [wsUrl])

  const { send } = useWebSocket({
    getUrl,
    enabled: termReady,
    reconnect: false,
    onOpen: (ws) => {
      const term = termRef.current
      if (!term) return
      term.writeln("\r\n\x1b[32mConnected to SSH terminal\x1b[0m\r\n")
      fitAddonRef.current?.fit()
      // Send initial size
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }))
    },
    onMessage: (e) => {
      termRef.current?.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data))
    },
    onError: () => {
      termRef.current?.writeln("\r\n\x1b[31mWebSocket error — connection failed\x1b[0m\r\n")
    },
    onClose: () => {
      termRef.current?.writeln("\r\n\x1b[33mConnection closed\x1b[0m\r\n")
    },
  })

  useEffect(() => {
    if (!containerRef.current) return
    const el = containerRef.current
    let mounted = true
    let resizeObserver: ResizeObserver | null = null

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

      termRef.current = term
      fitAddonRef.current = fitAddon

      term.onData((data) => {
        send(data)
      })

      term.onResize(({ cols, rows }) => {
        send(JSON.stringify({ type: "resize", cols, rows }))
      })

      // Observe container resize
      resizeObserver = new ResizeObserver(() => {
        if (mounted) fitAddon.fit()
      })
      resizeObserver.observe(el)

      // Terminal is ready — let useWebSocket open the connection
      setTermReady(true)
    }

    init().catch(console.error)

    return () => {
      mounted = false
      resizeObserver?.disconnect()
      termRef.current?.dispose()
      termRef.current = null
      fitAddonRef.current = null
      setTermReady(false)
    }
  }, [send])

  return (
    <div
      ref={containerRef}
      className={className}
      style={{ background: "#1e1e1e" }}
    />
  )
}
