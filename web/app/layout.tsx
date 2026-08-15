import type { Metadata, Viewport } from 'next'
import { Barlow, Barlow_Condensed } from 'next/font/google'
import { BrandingPageTitle } from '@/components/branding-page-title'
import './globals.css'

// Industry design system typography: Barlow for body copy, Barlow Condensed
// 600 for headings. Monospace is the system stack (see --font-mono in
// globals.css) so agent names, paths and ids match the terminal.
const barlow = Barlow({
  subsets: ['latin'],
  weight: ['400', '500', '700'],
  variable: '--font-barlow',
  display: 'swap',
})

const barlowCondensed = Barlow_Condensed({
  subsets: ['latin'],
  weight: ['400', '600'],
  variable: '--font-barlow-condensed',
  display: 'swap',
})

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
    <html
      lang="en"
      className={`dark h-full ${barlow.variable} ${barlowCondensed.variable}`}
    >
      <body className="font-sans antialiased h-full overflow-hidden">
        <BrandingPageTitle />
        {children}
      </body>
    </html>
  )
}
