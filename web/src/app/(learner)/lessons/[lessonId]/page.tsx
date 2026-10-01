'use client'

import { use, useCallback, useEffect, useState } from 'react'
import Link from 'next/link'
import { ApiError, api } from '@/lib/api'
import { offlineQueue } from '@/lib/offline-queue'
import type { Lesson } from '@/lib/types'
import { Button, ErrorMessage, LoadingRegion, StatusPill } from '@/components/ui'

/**
 * A lesson view: the content, plus the two actions that record progress.
 *
 * The "mark complete" button is the interesting part. It submits a
 * lesson_completed event through the offline queue, so a learner who taps it
 * in a lift still records the completion, and tapping it twice is harmless.
 */
export default function LessonPage({
  params,
}: {
  params: Promise<{ lessonId: string }>
}) {
  const { lessonId } = use(params)
  const id = Number.parseInt(lessonId, 10)

  const [lesson, setLesson] = useState<Lesson | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [note, setNote] = useState<string | null>(null)

  const load = useCallback(
    async (signal: AbortSignal) => {
      if (!Number.isInteger(id) || id <= 0) {
        setError('That lesson does not exist.')
        setLoading(false)
        return
      }
      try {
        setLesson(await api.lesson(id, signal))
        setError(null)
      } catch (caught: unknown) {
        if (caught instanceof DOMException && caught.name === 'AbortError') {
          return
        }
        setError(
          caught instanceof ApiError
            ? caught.message
            : 'Could not reach the Neo Learn server.',
        )
      } finally {
        setLoading(false)
      }
    },
    [id],
  )

  useEffect(() => {
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [load])

  async function markComplete() {
    if (!Number.isInteger(id) || id <= 0) {
      return
    }
    setSubmitting(true)
    setNote(null)

    const { queued } = await offlineQueue.submit({ kind: 'lesson_completed', lessonId: id })
    setSubmitting(false)

    if (queued) {
      setNote(
        'Saved on this device. It will sync automatically when you are back online.',
      )
      // Reflect the local intent immediately so the learner sees the effect of
      // their tap even though the server has not confirmed it.
      setLesson((current) =>
        current === null
          ? current
          : {
              ...current,
              status: 'completed',
              progress: {
                ...(current.progress ?? {
                  questions_answered: 0,
                  questions_correct: 0,
                  marks_awarded: 0,
                  marks_available: 0,
                  percent: 100,
                }),
                status: 'completed',
                percent: 100,
              },
            },
      )
      return
    }

    setNote('Marked complete.')
  }

  if (loading) {
    return <LoadingRegion label="Loading lesson" />
  }

  if (error !== null) {
    return (
      <div className="flex flex-col gap-4">
        <ErrorMessage>{error}</ErrorMessage>
        <Link href="/" className="text-sm font-medium text-slate-700 underline">
          Back to my learning
        </Link>
      </div>
    )
  }

  if (lesson === null) {
    return null
  }

  return (
    <article className="flex max-w-2xl flex-col gap-6">
      <div>
        <Link href="/" className="text-sm font-medium text-slate-600 hover:text-slate-900">
          &larr; My learning
        </Link>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <h1 className="text-2xl font-bold tracking-tight text-slate-900">
            {lesson.title}
          </h1>
          <StatusPill status={lesson.status} />
        </div>
        {lesson.sms_eligible && (
          <p className="mt-2 text-sm text-slate-600">
            This lesson can also be delivered to your phone by SMS.
          </p>
        )}
      </div>

      {/*
        Rendered as pre-wrapped text rather than injected HTML. There is no
        sanitiser in the client, so treating lesson text as untrusted and
        escaping it by construction is the safe default; a markdown renderer
        with raw HTML disabled can replace this later.
      */}
      <div className="rounded-lg border border-slate-200 bg-white p-6">
        {lesson.body_markdown.split(/\n{2,}/).map((paragraph, index) => (
          <p key={index} className="whitespace-pre-wrap leading-7 text-slate-800 [&_a]:underline">
            {paragraph}
          </p>
        ))}
      </div>

      <div className="flex flex-col gap-3">
        {note !== null && (
          <p role="status" aria-live="polite" className="text-sm text-slate-700">
            {note}
          </p>
        )}
        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={() => void markComplete()} disabled={submitting}>
            {submitting ? 'Saving...' : 'Mark lesson complete'}
          </Button>
          {lesson.status === 'completed' && (
            <span className="text-sm text-slate-600">Already completed.</span>
          )}
        </div>
      </div>
    </article>
  )
}