'use client'

import { use, useCallback, useEffect, useState } from 'react'
import Link from 'next/link'
import { ApiError, api } from '@/lib/api'
import { offlineQueue } from '@/lib/offline-queue'
import type { Assessment, GradedAnswer, Question } from '@/lib/types'
import {
  EmptyState,
  ErrorMessage,
  LoadingRegion,
  ProgressBar,
  StatusPill,
} from '@/components/ui'

/**
 * The assessment view.
 *
 * Structurally this is the same surface an SMS session drives: one question,
 * choices labelled A-D, and a reply that is a single label. The web client just
 * renders it with buttons instead of asking the learner to type.
 *
 * Grading is server-side and is never inferred from the answer key, which the
 * API does not send. After a submission the server's verdict replaces the
 * learner's selection, so a learner cannot be shown a result the platform
 * disagrees with.
 */
export default function AssessmentPage({
  params,
}: {
  params: Promise<{ assessmentId: string }>
}) {
  const { assessmentId } = use(params)
  const id = Number.parseInt(assessmentId, 10)

  const [assessment, setAssessment] = useState<Assessment | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  // Per-question feedback, keyed by question id.
  const [feedback, setFeedback] = useState<Record<number, GradedAnswer>>({})
  const [queuedNotes, setQueuedNotes] = useState<Record<number, string>>({})
  const [submitting, setSubmitting] = useState<number | null>(null)

  const load = useCallback(
    async (signal: AbortSignal) => {
      if (!Number.isInteger(id) || id <= 0) {
        setError('That assessment does not exist.')
        setLoading(false)
        return
      }
      try {
        setAssessment(await api.assessment(id, signal))
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

  async function answer(question: Question, label: string) {
    setSubmitting(question.id)

    const { graded, queued } = await offlineQueue.submit({
      kind: 'question_answered',
      questionId: question.id,
      assessmentId: id,
      choiceLabel: label,
    })

    setSubmitting(null)

    if (graded !== null) {
      setFeedback((current) => ({ ...current, [question.id]: graded }))
      // Refresh the totals so the header reflects the confirmed answer.
      void load(new AbortController().signal)
      return
    }

    if (queued) {
      setQueuedNotes((current) => ({
        ...current,
        [question.id]:
          'Answer saved on this device. It will be graded when you reconnect.',
      }))
      return
    }

    setQueuedNotes((current) => ({
      ...current,
      [question.id]: 'The server rejected that answer. It has not been recorded.',
    }))
  }

  if (loading) {
    return <LoadingRegion label="Loading assessment" />
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

  if (assessment === null) {
    return null
  }

  return (
    <div className="flex max-w-2xl flex-col gap-6">
      <div>
        <Link href="/" className="text-sm font-medium text-slate-600 hover:text-slate-900">
          &larr; My learning
        </Link>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <h1 className="text-2xl font-bold tracking-tight text-slate-900">
            {assessment.title}
          </h1>
          <StatusPill status={assessment.status} />
        </div>
        {assessment.description !== '' && (
          <p className="mt-1 text-sm text-slate-600">{assessment.description}</p>
        )}
        {assessment.sms_eligible && (
          <p className="mt-1 text-sm text-slate-600">
            You can also answer these questions by texting a single letter.
          </p>
        )}
        {assessment.marks_available > 0 && (
          <div className="mt-4 max-w-48">
            <ProgressBar
              percent={assessment.percent}
              label={`${assessment.title} score`}
            />
          </div>
        )}
      </div>

      {assessment.questions.length === 0 ? (
        <EmptyState
          title="No questions yet"
          description="This assessment has been created but no questions have been added."
        />
      ) : (
        <ol className="flex flex-col gap-5">
          {assessment.questions.map((question, index) => (
            <li key={question.id}>
              <QuestionCard
                question={question}
                index={index}
                feedback={feedback[question.id]}
                queuedNote={queuedNotes[question.id]}
                submitting={submitting === question.id}
                onAnswer={(label) => void answer(question, label)}
              />
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

function QuestionCard({
  question,
  index,
  feedback,
  queuedNote,
  submitting,
  onAnswer,
}: {
  question: Question
  index: number
  feedback: GradedAnswer | undefined
  queuedNote: string | undefined
  submitting: boolean
  onAnswer: (label: string) => void
}) {
  const selected = feedback?.choice_label ?? question.answered_label
  const answered = selected !== undefined

  return (
    <fieldset
      className="rounded-lg border border-slate-200 bg-white p-5"
      disabled={submitting}
    >
      <legend className="sr-only">
        Question {index + 1} of the assessment
      </legend>

      <p className="text-xs font-semibold uppercase tracking-wide text-slate-500">
        Question {index + 1} · {question.marks} mark{question.marks === 1 ? '' : 's'}
      </p>
      <p className="mt-2 font-medium leading-7 text-slate-900">{question.prompt}</p>

      <div className="mt-4 flex flex-col gap-2">
        {question.choices.map((choice) => {
          const isSelected = choice.label === selected
          return (
            <label
              key={choice.label}
              className={`flex cursor-pointer items-center gap-3 rounded-md border px-4 py-3 text-sm transition-colors has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-slate-500 ${
                isSelected
                  ? 'border-slate-900 bg-slate-50'
                  : 'border-slate-200 hover:bg-slate-50'
              }`}
            >
              <input
                type="radio"
                name={`question-${question.id}`}
                value={choice.label}
                checked={isSelected}
                onChange={() => onAnswer(choice.label)}
                className="h-4 w-4 shrink-0 accent-slate-900"
              />
              <span className="font-semibold text-slate-700">{choice.label}.</span>
              <span className="text-slate-800">{choice.body}</span>
            </label>
          )
        })}
      </div>

      {/*
        aria-live on the verdict: the answer arrives without a page change,
        so a screen reader user would otherwise get no confirmation.
      */}
      <div aria-live="polite" className="mt-3 min-h-5">
        {feedback !== undefined && (
          <p className={`text-sm font-medium ${feedback.correct ? 'text-emerald-800' : 'text-red-800'}`}>
            {feedback.correct
              ? `Correct. ${feedback.marks_awarded} mark awarded.`
              : `Not correct. The answer was ${feedback.choice_label}.`}
          </p>
        )}
        {queuedNote !== undefined && (
          <p className="text-sm text-amber-800">{queuedNote}</p>
        )}
        {feedback === undefined && queuedNote === undefined && answered && (
          <p className="text-sm text-slate-600">
            Answered {selected}. Re-answering replaces it.
          </p>
        )}
      </div>
    </fieldset>
  )
}