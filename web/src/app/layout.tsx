import type { Metadata, Viewport } from 'next'
import './globals.css'

export const metadata: Metadata = {
  title: 'Neo Learn',
  description:
    'A learning platform that keeps working when the connection does not. Lessons, assessments, and progress over the web or by SMS.',
}

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  // Not capped: capping zoom breaks the accessibility guidance for anyone who
  // needs to magnify text.
  maximumScale: 5,
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      {/*
        skipLink is the first focusable element on the page, so a keyboard user
        can jump past the navigation without tabbing through it.
      */}
      <body className="min-h-screen antialiased">
        <a
          href="#main"
          className="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-50 focus:rounded-md focus:bg-white focus:px-4 focus:py-2 focus:text-sm focus:font-semibold focus:shadow-lg"
        >
          Skip to main content
        </a>
        {children}
      </body>
    </html>
  )
}