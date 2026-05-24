/** @type {import('next').NextConfig} */
const isDev = process.env.NODE_ENV === "development"

const nextConfig = {
  // Static export for production (embedded in Go binary).
  // In dev (npm run dev), skip static export so route handlers + middleware work.
  ...(isDev ? {} : { output: "export", trailingSlash: true }),
  images: {
    unoptimized: true,
  },
  ...(isDev
    ? {
        async rewrites() {
          const hubUrl = process.env.NEXT_PUBLIC_HUB_URL || process.env.HUB_URL || "http://localhost:8080"
          return [
            {
              source: "/api/:path*",
              destination: `${hubUrl}/api/:path*`,
            },
          ]
        },
      }
    : {}),
}

export default nextConfig
