import type { Metadata } from 'next'
import { Geist, Geist_Mono } from 'next/font/google'
import { BrandingPageTitle } from '@/components/branding-page-title'
import { Toaster } from '@/components/ui/sonner'
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
        <Toaster />
      </body>
    </html>
  )
}
