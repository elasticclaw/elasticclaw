import { fileURLToPath } from "node:url"
import { defineConfig } from "vitest/config"

// Mirrors the "@/*" path alias from tsconfig.json so unit tests can import
// modules the same way application code does.
export default defineConfig({
  resolve: {
    alias: {
      "@": fileURLToPath(new URL(".", import.meta.url)),
    },
  },
})
