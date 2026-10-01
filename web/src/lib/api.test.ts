import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, newEventId } from './api'

/**
 * Tests for the API client.
 *
 * The assertions that matter are about credentials and error mapping, not
 * about URLs: getting those wrong is what would silently break sessions or
 * surface a raw server stack trace to a learner.
 */

interface FetchOptions {
  method?: string
  credentials?: string
  headers?: Record<string, string>
  body?: string
}

/**
 * The subset of Response the client actually uses.
 *
 * Declared structurally rather than casting to Response: a partial cast from
 * `string` to the body property fails typecheck, which is a useful reminder
 * that the stub is not a real Response.
 */
interface ResponseStub {
  ok: boolean
  status: number
  text: () => Promise<string>
}

function mockFetch(
  response: { status?: number; body?: string },
): ReturnType<typeof vi.fn> {
  const status = response.status ?? 200
  const stub: ResponseStub = {
    ok: status >= 200 && status < 300,
    status,
    text: () => Promise.resolve(response.body ?? ''),
  }
  return vi.fn().mockResolvedValue(stub as unknown as Response)
}

describe('api', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  describe('credentials', () => {
    it('sends the session cookie on every request', async () => {
      const fetchMock = mockFetch({ body: '{"courses":[]}' })
      vi.stubGlobal('fetch', fetchMock)

      await api.courses()

      const [, options] = fetchMock.mock.calls[0] as [string, FetchOptions]
      // Without this the browser never stores or returns the session cookie
      // and every learner is silently signed out.
      expect(options.credentials).toBe('include')
    })

    it('sends credentials on a POST, not just a GET', async () => {
      const fetchMock = mockFetch({ body: '{"event_id":"x","duplicate":false}' })
      vi.stubGlobal('fetch', fetchMock)

      await api.ingestEvent({ eventId: 'x', kind: 'lesson_started' })

      const [, options] = fetchMock.mock.calls[0] as [string, FetchOptions]
      expect(options.credentials).toBe('include')
      expect(options.method).toBe('POST')
    })
  })

  describe('error mapping', () => {
    it('surfaces the server error code and message', async () => {
      vi.stubGlobal(
        'fetch',
        mockFetch({
          status: 400,
          body: JSON.stringify({ error: 'choice_label is required', code: 'invalid_event' }),
        }),
      )

      await expect(
        api.ingestEvent({ eventId: 'x', kind: 'question_answered' }),
      ).rejects.toMatchObject({
        status: 400,
        code: 'invalid_event',
        message: 'choice_label is required',
      })
    })

    it('preserves per-field details for form rendering', async () => {
      vi.stubGlobal(
        'fetch',
        mockFetch({
          status: 400,
          body: JSON.stringify({
            error: 'missing credentials',
            code: 'missing_credentials',
            details: { email: 'required' },
          }),
        }),
      )

      try {
        await api.login('school', '', '')
        expect.unreachable('login should have failed')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).details).toEqual({ email: 'required' })
      }
    })

    it('classifies a transport failure as a network error, not an HTTP status', async () => {
      // fetch rejects rather than resolving when the server is unreachable.
      // That has to be distinguishable, since the offline queue keeps entries
      // queued only for this case.
      vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

      try {
        await api.courses()
        expect.unreachable('courses() should have failed')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).isNetworkError).toBe(true)
        expect((error as ApiError).status).toBe(0)
      }
    })

    it('does not treat an abort as a network failure', async () => {
      const abort = new DOMException('The operation was aborted.', 'AbortError')
      vi.stubGlobal('fetch', vi.fn().mockRejectedValue(abort))

      await expect(api.courses()).rejects.toBe(abort)
    })

    it('does not mark a 401 as a network error', async () => {
      vi.stubGlobal(
        'fetch',
        mockFetch({ status: 401, body: '{"error":"sign in to continue","code":"unauthenticated"}' }),
      )

      try {
        await api.me()
        expect.unreachable('me() should have failed')
      } catch (error) {
        expect((error as ApiError).isUnauthenticated).toBe(true)
        expect((error as ApiError).isNetworkError).toBe(false)
      }
    })

    it('handles an unparseable success body without throwing a syntax error at the caller', async () => {
      vi.stubGlobal('fetch', mockFetch({ status: 200, body: '<html>proxy error</html>' }))

      await expect(api.courses()).rejects.toMatchObject({ code: 'invalid_response' })
    })

    it('treats 204 as a successful empty result', async () => {
      vi.stubGlobal('fetch', mockFetch({ status: 204, body: '' }))

      await expect(api.logout()).resolves.toBeUndefined()
    })
  })

  describe('newEventId', () => {
    it('produces distinct ids across calls', () => {
      // The server enforces global uniqueness on event_id, so a client that
      // repeats itself would have its actions swallowed as duplicates.
      const ids = new Set(Array.from({ length: 500 }, () => newEventId()))
      expect(ids.size).toBe(500)
    })

    it('produces syntactically valid UUIDs', () => {
      const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
      for (let i = 0; i < 50; i += 1) {
        expect(newEventId()).toMatch(uuidV4)
      }
    })

    it('uses crypto.randomUUID where available', () => {
      const spy = vi.spyOn(crypto, 'randomUUID').mockReturnValue(
        '00000000-0000-4000-8000-000000000000',
      )
      expect(newEventId()).toBe('00000000-0000-4000-8000-000000000000')
      expect(spy).toHaveBeenCalled()
    })
  })
})