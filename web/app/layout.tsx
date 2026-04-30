import type { Metadata } from 'next'
import { Geist, Geist_Mono } from 'next/font/google'
import { BrandingPageTitle } from '@/components/branding-page-title'
import './globals.css'

const _geist = Geist({ subsets: ["latin"] });
const _geistMono = Geist_Mono({ subsets: ["latin"] });

export const metadata: Metadata = {
  title: 'ElasticClaw',
  description: 'Control plane for managing AI agent instances',
  generator: 'v0.app',
  icons: {
    icon: [
      {
        url: '/icon-light-32x32.png',
        media: '(prefers-color-scheme: light)',
      },
      {
        url: '/icon-dark-32x32.png',
        media: '(prefers-color-scheme: dark)',
      },
      {
        url: '/icon.svg',
        type: 'image/svg+xml',
      },
    ],
    apple: '/apple-icon.png',
  },
}

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode
}>) {
  return (
    <html lang="en" className="dark h-full">
      <body className="font-sans antialiased h-full overflow-hidden">
        <BrandingPageTitle />
        {children}
      </body>
    </html>
  )
}
