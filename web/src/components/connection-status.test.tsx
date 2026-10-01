import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { OfflineQueue } from '@/lib/offline-queue'
import { ConnectionStatus } from './connection-status'

/**
 * These tests pin the behaviour a learner depends on when the network fails:
 * that the banner says their work is safe, and that the server's rejection is
 * explained rather than swallowed.
 *
 * The queue is injected as a prop so each test owns its instance. The module
 * singleton would stay locked after any test that leaves a request in flight.
 */

function renderStatus(queue: OfflineQueue) {
  return render(<ConnectionStatus queue={queue} />)
}

/** Seeds the queue by simulating a failed submission. */
async function seedFailure(queue: OfflineQueue): Promise<void> {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
  await queue.submit({ kind: 'question_answered', questionId: 7, choiceLabel: 'B' })
}

describe('ConnectionStatus', () => {
  let queue: OfflineQueue

  beforeEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
    queue = new OfflineQueue()
  })

  it('renders nothing when connected with nothing pending', async () => {
    const online = vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(true)

    const { container } = renderStatus(queue)
    expect(container).toBeEmptyDOMElement()

    online.mockRestore()
  })

  it('tells the learner their answer is safe when offline', async () => {
    // The product promise is that losing connectivity does not lose work. A
    // learner who is not told that will stop answering.
    await seedFailure(queue)
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false)

    renderStatus(queue)

    expect(
      screen.getByText(/1 answer is saved on this device/),
    ).toBeInTheDocument()
    expect(screen.getByText(/will be sent automatically/i)).toBeInTheDocument()
  })

  it('pluralises the pending count', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await queue.submit({ kind: 'question_answered', questionId: 1 })
    await queue.submit({ kind: 'question_answered', questionId: 2 })
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false)

    renderStatus(queue)

    // Both submissions are queued, including the one retried by the second
    // flush, so the learner is told about every answer they are owed credit for.
    expect(screen.getByText(/2 answers are saved/)).toBeInTheDocument()
  })

  it('uses plural phrasing for two or more rejections', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        text: () =>
          Promise.resolve(JSON.stringify({ error: 'no such choice', code: 'choice_not_found' })),
      } as unknown as Response),
    )

    await queue.submit({ kind: 'question_answered', questionId: 1, choiceLabel: 'Z' })
    await queue.submit({ kind: 'question_answered', questionId: 2, choiceLabel: 'Z' })

    renderStatus(queue)

    expect(screen.getByText(/were rejected by the server/i)).toBeInTheDocument()
  })

  it('explains a server rejection with its reason rather than dropping it silently', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        text: () =>
          Promise.resolve(JSON.stringify({ error: 'no such choice', code: 'choice_not_found' })),
      } as unknown as Response),
    )

    await queue.submit({ kind: 'question_answered', questionId: 4, choiceLabel: 'Z' })
    renderStatus(queue)

    expect(screen.getByText(/rejected by the server/i)).toBeInTheDocument()
    expect(screen.getByText(/choice_not_found/)).toBeInTheDocument()
    // The learner can see which answer was lost, not just that something was.
    expect(screen.getByText(/Question 4/)).toBeInTheDocument()
  })

  it('lets the learner discard a rejected answer', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        text: () =>
          Promise.resolve(JSON.stringify({ error: 'no such choice', code: 'choice_not_found' })),
      } as unknown as Response),
    )

    await queue.submit({ kind: 'question_answered', questionId: 4, choiceLabel: 'Z' })

    const user = userEvent.setup()
    renderStatus(queue)

    await user.click(screen.getByRole('button', { name: /discard these/i }))

    // A permanently stuck entry would block every answer behind it.
    await waitFor(() => {
      expect(queue.snapshot().pending).toHaveLength(0)
    })
  })

  it('announces itself in a live region', async () => {
    await seedFailure(queue)
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false)

    renderStatus(queue)

    // The banner appears without a navigation, so a screen reader user needs
    // it announced.
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite')
  })

  it('offers a manual retry for queued answers', async () => {
    await seedFailure(queue)
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false)

    renderStatus(queue)

    expect(
      screen.getByRole('button', { name: /try sending now/i }),
    ).toBeInTheDocument()
  })
})