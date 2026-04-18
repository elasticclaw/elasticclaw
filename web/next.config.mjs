/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
  typescript: {
    ignoreBuildErrors: true,
  },
  images: {
    unoptimized: true,
  },
  async rewrites() {
    // Use server-side env vars (not NEXT_PUBLIC_*) so hub token is never exposed to browser
    const hubUrl = process.env.ELASTICCLAW_HUB_URL || process.env.HUB_URL || process.env.NEXT_PUBLIC_HUB_URL || "http://localhost:18788"
    return [
      {
        source: "/hub/:path*",
        destination: `${hubUrl}/:path*`,
      },
    ]
  },
  async headers() {
    return []
  },
}

export default nextConfig
