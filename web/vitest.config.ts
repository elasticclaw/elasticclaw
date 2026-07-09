import path from "node:path"
import { defineConfig } from "vitest/config"

export default defineConfig({
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "."),
    },
  },
  test: {
    // Pure-function tests run in node; DOM-dependent suites opt into jsdom
    // with a `// @vitest-environment jsdom` docblock.
    environment: "node",
    include: ["**/__tests__/**/*.{test,spec}.{ts,tsx}"],
  },
})
