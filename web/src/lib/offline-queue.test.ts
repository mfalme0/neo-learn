import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './api'
import { OfflineQueue, type QueuedEvent } from './offline-queue'

/**
 * Tests for the offline queue.
 *
 * The queue is the client half of the idempotency contract, so these tests
 * assert the two properties that matter: a failed send keeps the learner's
 * work, and a retried send does not duplicate it.
 */

const STORAGE_KEY = 'neo-learn.queue.v1'

function readStored(): QueuedEvent[] {
  const raw = localStorage.getItem(STORAGE_KEY)
  return raw === null ? [] : (JSON.parse(raw) as QueuedEvent[])
}

function storeEvents(events: QueuedEvent[]): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(events))
}

function pendingEvent(overrides: Partial<QueuedEvent> = {}): QueuedEvent {
  return {
    eventId: '11111111-1111-4111-8111-111111111111',
    kind: 'question_answered',
    questionId: 1,
    choiceLabel: 'B',
    occurredAt: '2026-01-01T09:00:00.000Z',
    attempts: 0,
    ...overrides,
  }
}

interface ResponseStub {
  ok: boolean
  status: number
  text: () => Promise<string>
}

/** mockIngest stubs fetch so each sent event resolves to `implementation`'s result. */
function mockIngest(
  implementation: (body: Record<string, unknown>) => Promise<unknown> | unknown,
): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (_url: string, options: { body?: string }) => {
    const body = JSON.parse(options.body ?? '{}') as Record<string, unknown>
    const result = await implementation(body)
    const stub: ResponseStub = {
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify(result)),
    }
    return stub as unknown as Response
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('offlineQueue', () => {
  let queue: OfflineQueue

  beforeEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
    // A fresh instance per test: the module singleton stays locked if any
    // other test leaves a request in flight, which would make these
    // assertions order-dependent.
    queue = new OfflineQueue()
  })

  describe('persistence', () => {
    it('writes the submission before attempting to send it', async () => {
      // If the process dies between the tap and the response, the answer must
      // already be on disk. The send is left hanging so the write can be
      // observed before it resolves.
      mockIngest(() => new Promise(() => {}))

      void queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'B' })

      // The write happens synchronously inside submit, before the first await.
      await vi.waitFor(() => {
        expect(readStored()).toHaveLength(1)
      })
    })

    it('persists the learner clock, not the send time', async () => {
      mockIngest(() => new Promise(() => {}))
      const before = Date.now()

      void queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'B' })

await vi.waitFor(() => {
        const entry = readStored()[0]
        expect(entry).toBeDefined()
        if (entry === undefined) {
          return
        }
        const occurred = new Date(entry.occurredAt).getTime()
        // The server derives offline lag from this, so it must reflect when
        // the learner acted.
        expect(occurred).toBeGreaterThanOrEqual(before - 1000)
      })
    })

    it('assigns each submission a fresh event id', async () => {
      mockIngest(() => new Promise(() => {}))

      void queue.submit({ kind: 'lesson_started', lessonId: 1 })
      void queue.submit({ kind: 'lesson_completed', lessonId: 1 })

await vi.waitFor(() => {
        const [first, second] = readStored()
        expect(first).toBeDefined()
        expect(second).toBeDefined()
        // A repeated id would make the server treat two distinct taps as one
        // action, silently dropping the second.
        if (first !== undefined && second !== undefined) {
          expect(first.eventId).not.toBe(second.eventId)
        }
      })
    })
  })

  describe('wedged request recovery', () => {
    it('does not stay locked when a send never settles', async () => {
      // The regression this guards: a request that hangs forever used to leave
      // the queue's in-flight lock set, so every later submission was written
      // to storage and silently never attempted. The learner would see a
      // banner that never clears and no sync, ever.
      vi.useFakeTimers()

      vi.stubGlobal(
        'fetch',
        vi.fn().mockImplementation(
          (_url: string, options: { signal?: AbortSignal }) =>
            new Promise<Response>((_resolve, reject) => {
              options.signal?.addEventListener('abort', () => {
                reject(new DOMException('Aborted', 'AbortError'))
              })
            }),
        ),
      )

      const first = queue.flush()
      await vi.advanceTimersByTimeAsync(16_000)
      await first

      vi.useRealTimers()

      // The lock released, so a later submission is actually attempted.
      const fetchMock = mockIngest(() => ({ event_id: 'x', duplicate: false }))
      await queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'B' })

      expect(fetchMock).toHaveBeenCalled()
    })

    it('keeps the timed-out entry queued for a later attempt', async () => {
      vi.useFakeTimers()
      vi.stubGlobal(
        'fetch',
        vi.fn().mockImplementation(
          (_url: string, options: { signal?: AbortSignal }) =>
            new Promise<Response>((_resolve, reject) => {
              options.signal?.addEventListener('abort', () => {
                reject(new DOMException('Aborted', 'AbortError'))
              })
            }),
        ),
      )

      storeEvents([pendingEvent()])
      const flushing = queue.flush()
      await vi.advanceTimersByTimeAsync(16_000)
      await flushing
      vi.useRealTimers()

      // A timeout is not a rejection: the learner's answer is still theirs.
      expect(readStored()).toHaveLength(1)
    })
  })

  describe('offline behaviour', () => {
    it('keeps the answer queued when the network is unreachable', async () => {
      vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

      const { queued } = await queue.submit({
        kind: 'question_answered',
        questionId: 1,
        choiceLabel: 'B',
      })

      expect(queued).toBe(true)
      expect(readStored()).toHaveLength(1)
    })

it('increments the attempt count so backoff can grow', async () => {
      vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

      await queue.submit({ kind: 'question_answered', questionId: 1 })
      await queue.submit({ kind: 'question_answered', questionId: 2 })

      // The first entry has been retried by the second flush, so it has two
      // attempts against the newer entry's one. That is the intended
      // behaviour: every flush drains the whole queue.
      expect(readStored().map((e) => e.attempts)).toEqual([2, 1])
    })

    it('caps the backoff delay', async () => {
      storeEvents([pendingEvent({ attempts: 40 })])

      // A learner in a lift must not have their device retrying every second
      // for five minutes.
      expect(queue.backoffMs()).toBe(60_000)
    })

    it('grows the backoff exponentially from the base delay', async () => {
      storeEvents([pendingEvent({ attempts: 0 })])
      expect(queue.backoffMs()).toBe(1_000)

      storeEvents([pendingEvent({ attempts: 3 })])
      expect(queue.backoffMs()).toBe(8_000)
    })

    it('flushes every queued entry once connectivity returns', async () => {
      storeEvents([
        pendingEvent({ eventId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', questionId: 1 }),
        pendingEvent({ eventId: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', questionId: 2 }),
      ])

      const fetchMock = mockIngest(() => ({ event_id: 'x', duplicate: false }))
      await queue.flush()

      expect(fetchMock).toHaveBeenCalledTimes(2)
      expect(readStored()).toHaveLength(0)
    })

    it('reuses the original event id when retrying, so the server deduplicates', async () => {
      const original = pendingEvent({ eventId: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc' })
      storeEvents([original])

      mockIngest(() => ({ event_id: original.eventId, duplicate: true }))
      await queue.flush()

      const [, options] = vi.mocked(fetch).mock.calls[0] as [string, { body?: string }]
      const sent = JSON.parse(options.body ?? '{}') as { event_id: string }
      // A new id here would be scored as a second answer.
      expect(sent.event_id).toBe('cccccccc-cccc-4ccc-8ccc-cccccccccccc')
    })

    it('preserves the learner clock across a retry', async () => {
      storeEvents([pendingEvent({ occurredAt: '2026-01-01T09:00:00.000Z' })])
      mockIngest(() => ({ event_id: 'x', duplicate: true }))

      await queue.flush()

      const [, options] = vi.mocked(fetch).mock.calls[0] as [string, { body?: string }]
      const sent = JSON.parse(options.body ?? '{}') as { occurred_at: string }
      expect(sent.occurred_at).toBe('2026-01-01T09:00:00.000Z')
    })
  })

  describe('server rejection', () => {
    it('retains a client-error entry with the reason, rather than retrying forever', async () => {
      // An entry the server will always reject must not block the queue behind
      // it, but it must not vanish silently either.
      vi.stubGlobal(
        'fetch',
        vi.fn().mockResolvedValue({
          ok: false,
          status: 404,
          text: () =>
            Promise.resolve(
              JSON.stringify({ error: 'no such choice', code: 'choice_not_found' }),
            ),
        } as Response),
      )

      await queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'Z' })

const entry = readStored()[0]
      expect(entry).toBeDefined()
      expect(entry?.lastError).toContain('choice_not_found')
      expect(entry?.attempts).toBe(1)
    })

    it('retries a 500, which is transient', async () => {
      vi.stubGlobal(
        'fetch',
        vi.fn().mockResolvedValue({
          ok: false,
          status: 500,
          text: () => Promise.resolve(JSON.stringify({ error: 'boom', code: 'internal_error' })),
        } as Response),
      )

      await queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'B' })

      const [entry] = readStored()
      expect(entry?.lastError).toBeUndefined()
    })

    it('discards a rejected entry on request', async () => {
      storeEvents([pendingEvent({ lastError: 'choice_not_found: nope' })])
      const [entry] = readStored()

      queue.discard(entry?.eventId ?? '')

      expect(readStored()).toHaveLength(0)
    })
  })

  describe('duplicate responses', () => {
    it('treats a duplicate as success and clears the entry', async () => {
      // A duplicate means the server already has it: from the learner's point
      // of view the answer landed.
      storeEvents([pendingEvent()])
      mockIngest(() => ({ event_id: 'x', duplicate: true }))

      const outcomes = await queue.flush()

      expect(outcomes.size).toBe(1)
      expect(readStored()).toHaveLength(0)
    })

    it('returns the server grading so the UI can show the verdict', async () => {
      mockIngest(() => ({
        event_id: 'x',
        duplicate: false,
        graded: {
          question_id: 1,
          choice_label: 'B',
          correct: true,
          marks: 1,
          marks_awarded: 1,
          marks_total: 3,
          percent: 33,
        },
      }))

      const { graded, queued } = await queue.submit({
        kind: 'question_answered',
        questionId: 1,
        choiceLabel: 'B',
      })

      expect(queued).toBe(false)
      expect(graded?.correct).toBe(true)
      expect(readStored()).toHaveLength(0)
    })
  })

  describe('resilience', () => {
    it('ignores malformed stored entries instead of blocking the queue', async () => {
      // A corrupt entry would otherwise be unsendable and stall every entry
      // behind it.
      localStorage.setItem(
        STORAGE_KEY,
        JSON.stringify([{ nonsense: true }, pendingEvent({ eventId: 'dddddddd-dddd-4ddd-8ddd-dddddddddddd' })]),
      )

      const fetchMock = mockIngest(() => ({ event_id: 'x', duplicate: false }))
      await queue.flush()

      expect(fetchMock).toHaveBeenCalledTimes(1)
    })

    it('survives unparseable storage', () => {
      localStorage.setItem(STORAGE_KEY, 'not json at all')
      expect(() => queue.snapshot()).not.toThrow()
      expect(queue.snapshot().pending).toEqual([])
    })

    it('ignores non-array storage', () => {
      localStorage.setItem(STORAGE_KEY, '{"not":"an array"}')
      expect(queue.snapshot().pending).toEqual([])
    })

    it('does not run two flushes concurrently', async () => {
      storeEvents([pendingEvent()])
      let resolveFetch: (() => void) | undefined
      vi.stubGlobal(
        'fetch',
        vi.fn().mockImplementation(
          () =>
            new Promise<Response>((resolve) => {
              resolveFetch = () =>
                resolve({
                  ok: true,
                  status: 200,
                  text: () => Promise.resolve('{"event_id":"x","duplicate":false}'),
                } as Response)
            }),
        ),
      )

      const first = queue.flush()
      const second = queue.flush()

      // A second concurrent drain would send the same event twice
      // simultaneously, which is exactly the duplicate the design prevents.
      expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1)

      resolveFetch?.()
      await Promise.all([first, second])
    })
  })

  describe('error classification', () => {
    it('exposes network and client errors distinctly', () => {
      expect(new ApiError(0, { error: 'x', code: 'network_error' }).isNetworkError).toBe(true)
      expect(new ApiError(404, { error: 'x', code: 'choice_not_found' }).isNetworkError).toBe(
        false,
      )
    })
  })
})