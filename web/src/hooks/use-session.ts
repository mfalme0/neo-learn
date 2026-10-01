import { useEffect, useState } from 'react'
import { ApiError, api } from '@/lib/api'
import type { User } from '@/lib/types'

/**
 * Session state for the app shell.
 *
 * The session cookie is HttpOnly, so the client cannot read the session
 * directly. It learns who is signed in by asking the API, which is also the
 * only trustworthy answer about whether the session is still valid.
 */
export function useSession() {
  const [state, setState] = useState<{
    status: 'loading' | 'authenticated' | 'anonymous' | 'error'
    user: User | null
    error: string | null
  }>({ status: 'loading', user: null, error: null })

  useEffect(() => {
    const controller = new AbortController()

    api
      .me(controller.signal)
      .then((user) => {
        setState({ status: 'authenticated', user, error: null })
      })
      .catch((error: unknown) => {
        if (error instanceof DOMException && error.name === 'AbortError') {
          return
        }
        if (error instanceof ApiError && error.isUnauthenticated) {
          // Not an error: nobody is signed in yet.
          setState({ status: 'anonymous', user: null, error: null })
          return
        }
        setState({
          status: 'error',
          user: null,
          error:
            error instanceof ApiError
              ? error.message
              : 'Could not reach the Neo Learn server.',
        })
      })

    return () => {
      controller.abort()
    }
  }, [])

  const signOut = async () => {
    await api.logout().catch(() => {
      // A failed logout still clears the cookie locally; the server session is
      // revoked on its next expiry.
    })
    setState({ status: 'anonymous', user: null, error: null })
    window.location.assign('/login')
  }

  return { ...state, signOut }
}