/**
 * The offline action queue.
 *
 * A learner who loses connectivity mid-assessment must not lose their answers.
 * Every submission is written to local storage *before* the network is tried,
 * with a client-generated event id. If the send fails the entry stays queued
 * and is retried later.
 *
 * This is safe because of the server's contract, not because of anything clever
 * here: the same event id replayed any number of times is recorded once. So the
 * queue can retry aggressively without ever double-scoring a learner.
 *
 * Storage is localStorage rather than IndexedDB deliberately. The queue is a
 * short list of small objects that must be readable synchronously at startup;
 * an async store would mean a render where queued answers are briefly missing,
 * and a learner could see their work vanish and re-enter it.
 */

import { api, ApiError, newEventId } from './api'
import type { EventKind, GradedAnswer } from './types'

const STORAGE_KEY = 'neo-learn.queue.v1'
const LISTENER_EVENT = 'neo-learn:queue-changed'

/** A submission waiting to reach the server. */
export interface QueuedEvent {
  /** The idempotency key. Stable across every retry of this entry. */
  readonly eventId: string
  readonly kind: EventKind
  readonly questionId?: number
  readonly lessonId?: number
  readonly assessmentId?: number
  readonly choiceLabel?: string
  /** When the learner acted, not when the queue got around to sending it. */
  readonly occurredAt: string
  /** How many send attempts have failed. Drives the backoff. */
  attempts: number
  /** Set after a terminal failure so the UI can explain what was lost. */
  lastError?: string
}

export type QueueState = 'idle' | 'syncing' | 'offline' | 'error'

export interface QueueSnapshot {
  readonly pending: readonly QueuedEvent[]
  readonly state: QueueState
  /** The most recent server verdict, if a sync just completed. */
  readonly lastGraded?: GradedAnswer
}

/**
 * Exponential backoff, capped. A learner in a lift should not have their
 * device retrying the network every second for five minutes.
 */
const BASE_BACKOFF_MS = 1_000
const MAX_BACKOFF_MS = 60_000

/**
 * Upper bound on a single send attempt.
 *
 * Without this a request that never settles would hold the queue's lock
 * forever: `syncing` would stay true and every later submission -- including
 * ones made after connectivity returns -- would be written to storage and
 * never attempted. That failure is invisible to the learner, who would just
 * see a banner that never clears.
 */
const SEND_TIMEOUT_MS = 15_000

function isQueuedEvent(value: unknown): value is QueuedEvent {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const candidate = value as Record<string, unknown>
  return (
    typeof candidate.eventId === 'string' &&
    typeof candidate.kind === 'string' &&
    typeof candidate.occurredAt === 'string' &&
    typeof candidate.attempts === 'number'
  )
}

function readQueue(): QueuedEvent[] {
  if (typeof localStorage === 'undefined') {
    return []
  }
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === null) {
      return []
    }
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) {
      return []
    }
    // Anything unrecognised is dropped rather than guessed at: a malformed
    // entry cannot be sent, and keeping it would block the queue forever.
    return parsed.filter(isQueuedEvent)
  } catch {
    return []
  }
}

function writeQueue(events: readonly QueuedEvent[]): void {
  if (typeof localStorage === 'undefined') {
    return
  }
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(events))
  } catch {
    // A full or unavailable store (private mode, quota) must not break the
    // app. The submission is still sent directly by the caller.
  }
  window.dispatchEvent(new CustomEvent(LISTENER_EVENT))
}

export class OfflineQueue {
  private syncing = false

  /** Current queue contents. Safe to call during render. */
  snapshot(): QueueSnapshot {
    return { pending: readQueue(), state: this.syncing ? 'syncing' : 'idle' }
  }

  subscribe(listener: (snapshot: QueueSnapshot) => void): () => void {
    const handler = () => listener(this.snapshot())
    window.addEventListener(LISTENER_EVENT, handler)
    // Also react to connectivity: coming back online is exactly when a flush
    // becomes worthwhile.
    window.addEventListener('online', handler)
    return () => {
      window.removeEventListener(LISTENER_EVENT, handler)
      window.removeEventListener('online', handler)
    }
  }

  /**
   * Records a submission and tries to send it.
   *
   * Resolves with the server's verdict on success, including the grading. On a
   * transport failure it resolves with `null` and leaves the entry queued --
   * the caller should treat that as "saved, not yet confirmed" rather than as
   * a failure, because the learner's work is not lost.
   */
  async submit(input: {
    kind: EventKind
    questionId?: number
    lessonId?: number
    assessmentId?: number
    choiceLabel?: string
  }): Promise<{ graded: GradedAnswer | null; queued: boolean }> {
    const entry: QueuedEvent = {
      eventId: newEventId(),
      kind: input.kind,
      questionId: input.questionId,
      lessonId: input.lessonId,
      assessmentId: input.assessmentId,
      choiceLabel: input.choiceLabel,
      occurredAt: new Date().toISOString(),
      attempts: 0,
    }

    // Persisted first. If the send fails the work is already safe.
    writeQueue([...readQueue(), entry])

    const sent = await this.flush()
    const match = sent.get(entry.eventId)

    if (match === undefined) {
      // Still queued: the learner should see their answer held locally.
      return { graded: null, queued: true }
    }
    return { graded: match ?? null, queued: false }
  }

  /**
   * Attempts to drain the queue.
   *
   * Returns the grading for each entry that reached the server, keyed by event
   * id. A duplicate returns `null` in that map: the event was already recorded,
   * which is a success from the learner's point of view.
   *
   * Entries are sent sequentially. Order does not affect correctness -- the
   * server resolves conflicts by occurred_at -- but a learner watching a
   * multi-question submission benefits from seeing it land in order.
   */
  async flush(): Promise<Map<string, GradedAnswer | null>> {
    const outcomes = new Map<string, GradedAnswer | null>()
    if (this.syncing) {
      return outcomes
    }

    const queued = readQueue()
    if (queued.length === 0) {
      return outcomes
    }

    this.syncing = true
    try {
      const remaining: QueuedEvent[] = []

      for (const entry of queued) {
        // A deadline per attempt, so one stuck request cannot wedge the queue
        // for the life of the page.
        const controller = new AbortController()
        const timeout = setTimeout(() => controller.abort(), SEND_TIMEOUT_MS)

        try {
          const response = await api.ingestEvent({
            eventId: entry.eventId,
            kind: entry.kind,
            questionId: entry.questionId,
            lessonId: entry.lessonId,
            assessmentId: entry.assessmentId,
            choiceLabel: entry.choiceLabel,
            // The learner's original clock, not now: the server measures
            // offline lag from this, and a queued answer happened when it
            // happened.
            occurredAt: entry.occurredAt,
            signal: controller.signal,
          })
          outcomes.set(entry.eventId, response.graded ?? null)
        } catch (error) {
          const attempt = entry.attempts + 1
          const failed: QueuedEvent = { ...entry, attempts: attempt }

          if (isAbort(error)) {
            // Timed out. Treated like a transport failure: the event stays
            // queued and the next flush retries it.
            remaining.push(failed)
            continue
          }

          if (error instanceof ApiError && error.isNetworkError) {
            // Keep it. The next flush will retry.
            remaining.push(failed)
            continue
          }

          if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
            // The server rejected it and will reject it again: a bad choice
            // label, a question from another institution. Retrying forever
            // would block every entry behind it, so it is dropped with the
            // reason retained for the UI to surface.
            remaining.push({
              ...failed,
              lastError: `${error.code}: ${error.message}`,
            })
            continue
          }

          // 5xx or something unexpected: transient, so keep and retry.
          remaining.push(failed)
        } finally {
          clearTimeout(timeout)
        }
      }

      writeQueue(remaining)
      return outcomes
    } finally {
      this.syncing = false
    }
  }

  /** Discards a queued entry the server has permanently rejected. */
  discard(eventId: string): void {
    writeQueue(readQueue().filter((entry) => entry.eventId !== eventId))
  }

  /**
   * Milliseconds to wait before the next automatic flush.
   *
   * Exposed so the UI can schedule its own retry without duplicating the
   * backoff calculation.
   */
  backoffMs(): number {
    const queued = readQueue()
    const highestAttempt = queued.reduce((max, entry) => Math.max(max, entry.attempts), 0)
    return Math.min(BASE_BACKOFF_MS * 2 ** highestAttempt, MAX_BACKOFF_MS)
  }
}

/** isAbort reports whether an error came from an aborted request. */
function isAbort(error: unknown): boolean {
  return (
    (error instanceof DOMException && error.name === 'AbortError') ||
    (error instanceof Error && error.name === 'AbortError')
  )
}

/**
 * Process-wide queue instance.
 *
 * A module singleton rather than React context: the queue must survive
 * navigation, and a fresh instance per mount would lose pending entries.
 */
export const offlineQueue = new OfflineQueue()