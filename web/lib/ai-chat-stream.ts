// SSE frames may span network chunks, including the CRLF separator and UTF-8.
export async function readAIChatEvents(body: ReadableStream<Uint8Array>, onEvent: (event: string, data: Record<string, unknown>) => void): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  let event = ""
  let data: string[] = []
  let completed = false
  const line = (value: string) => {
    if (value === "") {
      if (data.length) {
        const payload: unknown = JSON.parse(data.join("\n"))
        if (payload && typeof payload === "object" && !Array.isArray(payload)) {
          onEvent(event, payload as Record<string, unknown>)
          if (event === "done") completed = true
        }
      }
      event = ""
      data = []
    } else if (value.startsWith("event:")) event = value.slice(6).trim()
    else if (value.startsWith("data:")) data.push(value.slice(5).replace(/^ /, ""))
  }
  try {
    while (!completed) {
      const chunk = await reader.read()
      buffer += decoder.decode(chunk.value, { stream: !chunk.done })
      let end: number
      while ((end = buffer.indexOf("\n")) !== -1) {
        line(buffer.slice(0, end).replace(/\r$/, ""))
        buffer = buffer.slice(end + 1)
      }
      if (chunk.done) break
    }
    if (!completed) throw new Error("The connection ended before the response finished. Try again.")
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}
