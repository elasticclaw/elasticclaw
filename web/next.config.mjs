/** @type {import('next').NextConfig} */
const isDev = process.env.NODE_ENV === "development"

const nextConfig = {
  // Static export for production (embedded in Go binary).
  // In dev (npm run dev), skip static export so route handlers + middleware work.
  ...(isDev ? {} : { output: "export" }),
  trailingSlash: true,
  images: {
    unoptimized: true,
  },
  // Dev only: proxy /hub/* and /api/* to the hub
  ...(isDev && {
    async rewrites() {
      const hubUrl =
        process.env.ELASTICCLAW_HUB_URL ||
        process.env.HUB_URL ||
        "http://localhost:8080"
      return [
        { source: "/hub/:path*", destination: `${hubUrl}/:path*` },
        { source: "/api/auth/:path*", destination: `${hubUrl}/api/auth/:path*` },
        { source: "/api/hub-config", destination: `${hubUrl}/api/hub-config` },
      ]
    },
  }),
}

export default nextConfig
