/**
 * Root route.
 *
 * A server component that redirects based on the session cookie's presence
 * cannot know whether the cookie is valid -- it is HttpOnly, and the API is
 * the only authority. So this only distinguishes "has a session cookie" from
 * "does not", and the client-side shell does the real check.
 */

import { redirect } from 'next/navigation'

/**
 * Root route.
 *
 * The session cookie is HttpOnly, so a server component cannot tell whether it
 * is valid -- only the API can. This therefore sends every visitor to sign in
 * and lets the client-side shell bounce them back once the session is
 * confirmed. Attempting to decide here would mean either exposing the token to
 * the server or duplicating the session check in the frontend.
 */
export default function RootPage() {
  redirect('/login')
}