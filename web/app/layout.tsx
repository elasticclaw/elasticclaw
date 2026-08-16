import type { Metadata, Viewport } from 'next'
import { Archivo, Geist_Mono } from 'next/font/google'
import { BrandingPageTitle } from '@/components/branding-page-title'
import './globals.css'

// Modernist type: Archivo for everything (400 body, 600 emphasis, 800
// headings), Geist Mono for agent names, steps and metrics. Loading them here
// registers the 'Archivo' / 'Geist Mono' families that globals.css names in
// its @theme block.
const _archivo = Archivo({ subsets: ['latin'], weight: ['400', '600', '800'] });
const _geistMono = Geist_Mono({ subsets: ["latin"] });

// viewportFit: 'cover' makes env(safe-area-inset-*) non-zero on notched
// phones so the composer and tab bar can pad above the home indicator.
export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  viewportFit: 'cover',
}

export const metadata: Metadata = {
  title: 'ElasticClaw',
  description: 'Control plane for managing AI agent instances',
  generator: 'v0.app',
  icons: {
    icon: [
      {
        url: '/icon-lightning-light-32x32.png',
        media: '(prefers-color-scheme: light)',
        type: 'image/png',
      },
      {
        url: '/icon-lightning-dark-32x32.png',
        media: '(prefers-color-scheme: dark)',
        type: 'image/png',
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
