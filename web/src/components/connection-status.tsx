'use client'

import { useQueue, useOnline } from '@/hooks/use-queue'
import { offlineQueue, type OfflineQueue } from '@/lib/offline-queue'
import { Button } from '@/components/ui'

/**
 * ConnectionStatus tells the learner the truth about their work.
 *
 * This exists because the product promise is that losing connectivity does not
 * lose progress. A learner who cannot see that their answers are safe will stop
 * answering. So the banner states what is held locally, not merely that
 * something is wrong.
 *
 * It is a live region because it appears and disappears on its own; polite
 * semantics avoid interrupting whatever a screen reader is currently saying.
 */
export function ConnectionStatus({ queue = offlineQueue }: { queue?: OfflineQueue }) {
  const { pending } = useQueue(queue)
  const online = useOnline()

  if (pending.length === 0 && online) {
    return null
  }

  const failed = pending.filter((entry) => entry.lastError !== undefined)

  if (failed.length > 0) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="border-b border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-950"
      >
        <p className="font-medium">
          {failed.length === 1
            ? 'One answer was rejected by the server.'
            : `${failed.length} answers were rejected by the server.`}
        </p>
        <ul className="mt-1 list-inside list-disc text-amber-900">
          {failed.map((entry) => (
            <li key={entry.eventId}>
              {entry.questionId !== undefined
                ? `Question ${entry.questionId}`
                : entry.kind.replace(/_/g, ' ')}
              : {entry.lastError}
            </li>
          ))}
        </ul>
        <div className="mt-2">
          <Button
            variant="secondary"
            onClick={() => {
              for (const entry of failed) {
                queue.discard(entry.eventId)
              }
            }}
          >
            Discard these
          </Button>
        </div>
      </div>
    )
  }

  if (pending.length > 0) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="border-b border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-950"
      >
        <p className="font-medium">
          {online ? 'Sending' : 'Offline'} —{' '}
          {pending.length === 1
            ? '1 answer is saved on this device and will be sent automatically.'
            : `${pending.length} answers are saved on this device and will be sent automatically.`}
        </p>
        <div className="mt-2">
          <Button variant="secondary" onClick={() => void queue.flush()}>
            Try sending now
          </Button>
        </div>
      </div>
    )
  }

  return (
    <div
      role="status"
      aria-live="polite"
      className="border-b border-slate-300 bg-slate-100 px-4 py-2 text-sm text-slate-800"
    >
      You are offline. You can keep working; answers will be sent when you
      reconnect.
    </div>
  )
}