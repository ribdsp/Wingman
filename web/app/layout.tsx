import type { ReactNode } from 'react'
import type { Metadata } from 'next'
import { Geist, Geist_Mono } from 'next/font/google'
import './globals.css'

/*
 * Geist for everything, Geist Mono for anything that is a number or an id.
 *
 * Rakazo's UI face, and the reason its screens read as a product rather than a report: a
 * grotesque with real character at 15px, wide enough apertures to stay legible in a dense
 * table, and a matching mono so a run id and the figure above it belong to the same family.
 * Both are variable, so one file covers every weight this console uses.
 *
 * Downloaded at build time and served from this origin. A self-hosted ops console that fetches
 * a font from a CDN renders differently when the network is broken, which is exactly the
 * moment somebody is reading it — and it would need a hole in the CSP to do it.
 */
const geist = Geist({
  subsets: ['latin'],
  variable: '--font-geist',
  display: 'swap',
})

const geistMono = Geist_Mono({
  subsets: ['latin'],
  variable: '--font-geist-mono',
  display: 'swap',
})

export const metadata: Metadata = {
  title: 'Wingman',
  description: 'Operator console for a self-hosted Wingman instance.',
  // An instance found on the internet should not also be findable in a search engine.
  robots: { index: false, follow: false },
}

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={`${geist.variable} ${geistMono.variable}`}>
      <body>{children}</body>
    </html>
  )
}
