import type { NextConfig } from 'next'

const nextConfig: NextConfig = {
  reactStrictMode: true,

  // The browser talks to the Go API directly, not through a Next.js proxy, so
  // the session cookie stays a first-party cookie and no server component has
  // to hold the session token.
  env: {
    NEXT_PUBLIC_API_BASE_URL: process.env.NEXT_PUBLIC_API_BASE_URL ?? 'http://localhost:8080',
  },

  // Belt and braces alongside the headers in middleware: everything under /app
  // is learner data and must never be cached by a shared cache.
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'same-origin' },
          { key: 'X-Frame-Options', value: 'DENY' },
        ],
      },
    ]
  },
}

export default nextConfig