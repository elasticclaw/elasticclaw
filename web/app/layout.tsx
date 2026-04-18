import type { Metadata } from 'next'
import './globals.css'

export const metadata: Metadata = {
  title: 'ElasticClaw',
  description: 'Control plane for managing AI agent instances',
  icons: {
    icon: [
      { url: '/icon-light-32x32.png', media: '(prefers-color-scheme: light)' },
      { url: '/icon-dark-32x32.png', media: '(prefers-color-scheme: dark)' },
      { url: '/icon.svg', type: 'image/svg+xml' },
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
        {children}
      </body>
    </html>
  )
}
