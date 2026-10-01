'use client'

import type { ReactNode } from 'react'
import Link from 'next/link'
import { useSession } from '@/hooks/use-session'
import { ConnectionStatus } from '@/components/connection-status'
import { Button, ErrorMessage, LoadingRegion } from '@/components/ui'

/**
 * AppShell is the authenticated layout: navigation, connection status, and
 * the sign-out control.
 *
 * It also guards the routes beneath it. The API enforces authentication
 * regardless, so this is about not showing a learner a broken page before
 * bouncing them to sign in.
 */
export function AppShell({ children }: { children: ReactNode }) {
  const { status, user, error, signOut } = useSession()

  if (status === 'loading') {
    return (
      <div className="mx-auto max-w-4xl px-4 py-10">
        <LoadingRegion label="Checking your session" />
      </div>
    )
  }

  if (status === 'error') {
    return (
      <div className="mx-auto max-w-md px-4 py-16">
        <ErrorMessage>
          {error ?? 'Could not reach the Neo Learn server.'} Check your
          connection and try again.
        </ErrorMessage>
      </div>
    )
  }

  if (status === 'anonymous') {
    // Replace rather than push, so the back button does not bounce a
    // signed-out learner between a protected page and the login form.
    if (typeof window !== 'undefined') {
      window.location.replace('/login')
    }
    return null
  }

  return (
    <div className="min-h-screen">
      <header className="border-b border-slate-200 bg-white">
        <div className="mx-auto flex max-w-4xl flex-wrap items-center justify-between gap-3 px-4 py-3">
          <Link href="/" className="text-lg font-bold tracking-tight text-slate-900">
            Neo Learn
          </Link>
          <nav aria-label="Main" className="flex items-center gap-4">
            <Link
              href="/"
              className="rounded text-sm font-medium text-slate-600 hover:text-slate-900"
            >
              My learning
            </Link>
            {user !== null && (
              <span className="hidden text-sm text-slate-500 sm:inline">
                {user.display_name}
              </span>
            )}
            <Button variant="ghost" onClick={() => void signOut()}>
              Sign out
            </Button>
          </nav>
        </div>
      </header>

      <ConnectionStatus />

      <main id="main" className="mx-auto max-w-4xl px-4 py-8">
        {children}
      </main>
    </div>
  )
}