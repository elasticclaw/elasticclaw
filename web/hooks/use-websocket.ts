"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import {
  WS_BASE_RECONNECT_DELAY_MS,
  WS_ERROR_LOG_THROTTLE_MS,
  WS_MAX_BACKOFF_EXPONENT,
  WS_MAX_RECONNECT_DELAY_MS,
} from "@/lib/constants"

/** Redacts credentials from a WebSocket URL so it is safe to log. */
export function describeWsUrl(rawUrl: string): string {
  try {
    const url = new URL(rawUrl)
    if (url.searchParams.has("token")) {
      url.searchParams.set("token", "[redacted]")
    }
    return url.toString()
  } catch {
    return rawUrl.replace(/token=[^&]+/, "token=[redacted]")
  }
}

export interface UseWebSocketOptions {
  /**
   * Builds the connection URL. Called on every (re)connect attempt so
   * rotating credentials (e.g. a refreshed token) are picked up.
   */
  getUrl: () => string
  /** The connection is only opened while `enabled` is true. */
  enabled: boolean
  /** Reconnect with exponential backoff when the socket closes. Default true. */
  reconnect?: boolean
  onOpen?: (ws: WebSocket) => void
  onMessage?: (event: MessageEvent, ws: WebSocket) => void
  onClose?: (event: CloseEvent) => void
  /**
   * Custom error handling. When omitted, errors are logged with the
   * redacted URL, throttled to one warning per WS_ERROR_LOG_THROTTLE_MS.
   */
  onError?: (event: Event) => void
}

export interface UseWebSocketResult {
  connected: boolean
  /** Sends if the socket is open; returns false (and drops the data) otherwise. */
  send: (data: string | ArrayBufferLike | Blob | ArrayBufferView) => boolean
}

/**
 * useWebSocket — owns a single WebSocket connection: lifecycle, exponential
 * backoff reconnection, and URL redaction in logs. Event handlers are read
 * through a ref, so passing new callback identities does not reconnect.
 */
export function useWebSocket(options: UseWebSocketOptions): UseWebSocketResult {
  const { getUrl, enabled, reconnect = true } = options
  const [connected, setConnected] = useState(false)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimerRef = useRef<number | null>(null)
  const reconnectAttemptRef = useRef(0)
  const shouldReconnectRef = useRef(false)
  const lastErrorLogRef = useRef(0)
  const optionsRef = useRef(options)

  useEffect(() => {
    optionsRef.current = options
  })

  useEffect(() => {
    if (!enabled) return
    shouldReconnectRef.current = true

    function connect() {
      if (!shouldReconnectRef.current) return
      if (wsRef.current) {
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
      }
      const wsUrl = optionsRef.current.getUrl()
      const safeWsUrl = describeWsUrl(wsUrl)
      let ws: WebSocket
      try {
        ws = new WebSocket(wsUrl)
      } catch (err) {
        console.error(`WS create failed for ${safeWsUrl}:`, err)
        return
      }
      wsRef.current = ws

      ws.onopen = () => {
        reconnectAttemptRef.current = 0
        setConnected(true)
        optionsRef.current.onOpen?.(ws)
      }

      ws.onclose = (event) => {
        if (wsRef.current !== ws) return
        setConnected(false)
        optionsRef.current.onClose?.(event)
        if (!shouldReconnectRef.current || optionsRef.current.reconnect === false) return
        const attempt = reconnectAttemptRef.current
        reconnectAttemptRef.current += 1
        const delayMs = Math.min(
          WS_MAX_RECONNECT_DELAY_MS,
          WS_BASE_RECONNECT_DELAY_MS * 2 ** Math.min(attempt, WS_MAX_BACKOFF_EXPONENT)
        )
        if (event.code !== 1000) {
          console.warn(
            `WS closed for ${safeWsUrl}: code=${event.code} reason=${event.reason || "none"}; reconnecting in ${Math.round(delayMs / 1000)}s`
          )
        }
        if (reconnectTimerRef.current) window.clearTimeout(reconnectTimerRef.current)
        reconnectTimerRef.current = window.setTimeout(connect, delayMs)
      }

      ws.onerror = (event) => {
        if (optionsRef.current.onError) {
          optionsRef.current.onError(event)
          return
        }
        const nowMs = Date.now()
        if (nowMs - lastErrorLogRef.current < WS_ERROR_LOG_THROTTLE_MS) return
        lastErrorLogRef.current = nowMs
        console.warn(`WS error for ${safeWsUrl}; check the Network tab for /api/ws status and close code`)
      }

      ws.onmessage = (event) => {
        optionsRef.current.onMessage?.(event, ws)
      }
    }

    connect()
    return () => {
      shouldReconnectRef.current = false
      if (reconnectTimerRef.current) window.clearTimeout(reconnectTimerRef.current)
      reconnectTimerRef.current = null
      if (wsRef.current) {
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
        wsRef.current = null
      }
      setConnected(false)
    }
    // getUrl identity change means "connect somewhere else" — reconnect.
  }, [enabled, getUrl, reconnect])

  const send = useCallback((data: string | ArrayBufferLike | Blob | ArrayBufferView): boolean => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(data)
      return true
    }
    return false
  }, [])

  return { connected, send }
}
