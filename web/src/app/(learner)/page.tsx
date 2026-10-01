'use client'

import { useCallback, useEffect, useState } from 'react'
import Link from 'next/link'
import { ApiError, api } from '@/lib/api'
import type { Assessment, CourseProgress, Module, ProgressStatus } from '@/lib/types'
import {
  EmptyState,
  ErrorMessage,
  LoadingRegion,
  ProgressBar,
  StatusPill,
} from '@/components/ui'

/**
 * The learner dashboard: enrolled courses with their progress, plus the
 * lessons and assessments inside each.
 *
 * Both panels load independently so a failure in one does not blank the other.
 * A learner in a weak-signal area should still see their lessons if the
 * assessment request times out.
 */
export default function DashboardPage() {
  const [modulesByCourse, setModulesByCourse] = useState<Record<number, Module[]>>({})
  const [assessmentsByCourse, setAssessmentsByCourse] = useState<Record<number, Assessment[]>>({})
  const [progress, setProgress] = useState<CourseProgress[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (signal: AbortSignal) => {
    try {
      const { courses } = await api.courses(signal)

      const [progressResult, ...courseResults] = await Promise.all([
        api.progress(signal),
        ...courses.map((course) =>
          Promise.all([
            api.courseModules(course.id, signal).then((r) => r.modules),
            api.assessments(course.id, signal).then((r) => r.assessments),
          ]),
        ),
      ])

      setProgress(progressResult.courses)

      const nextModules: Record<number, Module[]> = {}
      const nextAssessments: Record<number, Assessment[]> = {}
      courses.forEach((course, index) => {
        const [modules, assessments] = courseResults[index] ?? [[], []]
        nextModules[course.id] = modules
        nextAssessments[course.id] = assessments
      })
      setModulesByCourse(nextModules)
      setAssessmentsByCourse(nextAssessments)
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
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [load])

  if (loading) {
    return <LoadingRegion label="Loading your courses" />
  }

  if (error !== null) {
    return (
      <div className="flex flex-col gap-4">
        <ErrorMessage>{error}</ErrorMessage>
        <button
          type="button"
          onClick={() => {
            setLoading(true)
            void load(new AbortController().signal)
          }}
          className="self-start rounded-md border border-slate-300 px-4 py-2 text-sm font-semibold hover:bg-slate-50"
        >
          Try again
        </button>
      </div>
    )
  }

  const courses = progress ?? []

  if (courses.length === 0) {
    return (
      <EmptyState
        title="No courses yet"
        description="When a teacher assigns you a course, it will appear here. You can also receive lessons by SMS."
      />
    )
  }

  return (
    <div className="flex flex-col gap-8">
      <div>
        <h1 className="text-2xl font-bold tracking-tight text-slate-900">My learning</h1>
        <p className="mt-1 text-sm text-slate-600">
          Progress recorded from any device, whether you used the app or SMS.
        </p>
      </div>

      {courses.map((course) => {
        const modules = modulesByCourse[course.course_id] ?? []
        const assessments = assessmentsByCourse[course.course_id] ?? []

        return (
          <section
            key={course.course_id}
            aria-labelledby={`course-${course.course_id}`}
            className="rounded-lg border border-slate-200 bg-white p-5"
          >
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <p className="text-xs font-semibold uppercase tracking-wide text-slate-500">
                  {course.code}
                </p>
                <h2
                  id={`course-${course.course_id}`}
                  className="mt-0.5 text-lg font-semibold text-slate-900"
                >
                  {course.title}
                </h2>
              </div>
              <div className="w-full max-w-40">
                <ProgressBar
                  percent={course.percent}
                  label={`${course.title} overall progress`}
                />
              </div>
            </div>

            <dl className="mt-4 grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
              <div>
                <dt className="text-slate-500">Lessons completed</dt>
                <dd className="font-semibold text-slate-900">
                  {course.lessons_completed} of {course.lessons_total}
                </dd>
              </div>
              <div>
                <dt className="text-slate-500">Score</dt>
                <dd className="font-semibold text-slate-900">
                  {course.marks_awarded} / {course.marks_available}
                </dd>
              </div>
            </dl>

            {modules.length > 0 && (
              <div className="mt-6">
                <h3 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
                  Lessons
                </h3>
                <ul className="mt-2 flex flex-col gap-2">
                  {modules.map((module) =>
                    module.lessons.map((lesson) => (
                      <li key={lesson.id}>
                        <LessonRow
                          id={lesson.id}
                          title={lesson.title}
                          status={lesson.status}
                          smsEligible={lesson.sms_eligible}
                        />
                      </li>
                    )),
                  )}
                </ul>
              </div>
            )}

            {assessments.length > 0 && (
              <div className="mt-6">
                <h3 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
                  Assessments
                </h3>
                <ul className="mt-2 flex flex-col gap-2">
                  {assessments.map((assessment) => (
                    <li key={assessment.id}>
                      <AssessmentRow assessment={assessment} />
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </section>
        )
      })}
    </div>
  )
}

function LessonRow({
  id,
  title,
  status,
  smsEligible,
}: {
  id: number
  title: string
  status: ProgressStatus
  smsEligible: boolean
}) {
  return (
    <Link
      href={`/lessons/${id}`}
      className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-slate-200 px-4 py-3 hover:border-slate-300 hover:bg-slate-50"
    >
      <span className="font-medium text-slate-900">{title}</span>
      <span className="flex items-center gap-2">
        {smsEligible && (
          <span className="rounded bg-slate-100 px-2 py-0.5 text-xs text-slate-600">
            SMS
          </span>
        )}
        <StatusPill status={status} />
      </span>
    </Link>
  )
}

function AssessmentRow({ assessment }: { assessment: Assessment }) {
  return (
    <Link
      href={`/assessments/${assessment.id}`}
      className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-slate-200 px-4 py-3 hover:border-slate-300 hover:bg-slate-50"
    >
      <span>
        <span className="font-medium text-slate-900">{assessment.title}</span>
        {assessment.marks_available > 0 && (
          <span className="ml-2 text-sm text-slate-500">
            {assessment.marks_awarded} / {assessment.marks_available}
          </span>
        )}
      </span>
      <span className="flex items-center gap-2">
        {assessment.sms_eligible && (
          <span className="rounded bg-slate-100 px-2 py-0.5 text-xs text-slate-600">
            SMS
          </span>
        )}
        <StatusPill status={assessment.status} />
      </span>
    </Link>
  )
}