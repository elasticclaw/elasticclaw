/** @type {import('next').NextConfig} */
const isDev = process.env.NODE_ENV === "development"

const nextConfig = {
  // Static export for production (embedded in Go binary).
  // In dev (npm run dev), skip static export so route handlers + middleware work.
  ...(isDev ? {} : { output: "export", trailingSlash: true }),
  images: {
    unoptimized: true,
  },
  // No rewrites needed — in dev, the browser calls the hub directly via NEXT_PUBLIC_HUB_URL
  // In production, the hub serves the static files so everything is same-origin
}

export default nextConfig
