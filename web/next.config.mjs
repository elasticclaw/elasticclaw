/** @type {import('next').NextConfig} */

const isDev = process.env.NODE_ENV === "development"

const nextConfig = {
  // Static export for production (embedded in Go binary).
  // In dev (npm run dev), skip static export so rewrites + middleware work.
  ...(isDev ? {} : { output: "export" }),
  trailingSlash: true,
  images: {
    unoptimized: true,
  },
  // Dev only: proxy /hub/* to the hub API, and /api/* to hub auth endpoints.
  // In production these are served directly by the Go hub on the same port.
  ...(isDev && {
    async rewrites() {
      const hubUrl =
        process.env.ELASTICCLAW_HUB_URL ||
        process.env.HUB_URL ||
        "http://localhost:8080"
      return [
        {
          source: "/hub/:path*",
          destination: `${hubUrl}/:path*`,
        },
        {
          source: "/api/:path*",
          destination: `${hubUrl}/api/:path*`,
        },
      ]
    },
  }),
}

export default nextConfig
