/**
 * Types mirroring the Neo Learn REST API.
 *
 * These are hand-maintained rather than generated. The cost is a chance of
 * drift; the benefit is that the web client is a genuine consumer of the API
 * contract, which is what keeps that contract honest. If a field changes
 * server-side, this file should fail typecheck or the request should fail at
 * runtime -- both are the signal we want.
 *
 * Note what is absent: no `is_correct` on Choice, because the server never
 * sends it. The answer key is not available to the client before submission.
 */

export type UserRole = 'admin' | 'teacher' | 'student'

/** Mirrors the server's event_kind enum. */
export type EventKind =
  | 'lesson_started'
  | 'lesson_completed'
  | 'question_answered'
  | 'assessment_submitted'

/**
 * Mirrors the server's event_source enum.
 *
 * 'sms' and 'stk' are never sent by this client: the browser is not the
 * origin of an SMS reply. They appear because the same assessment is rendered
 * identically regardless of transport, and a learner switching devices should
 * see consistent state.
 */
export type EventSource = 'web' | 'sms' | 'stk'

export type ProgressStatus = 'not_started' | 'in_progress' | 'completed'

export interface User {
  readonly id: number
  readonly email: string
  readonly display_name: string
  readonly role: UserRole
  readonly institution_slug: string
}

export interface Course {
  readonly id: number
  readonly code: string
  readonly title: string
  readonly description: string
}

export interface LessonSummary {
  readonly id: number
  readonly module_id: number
  readonly title: string
  /** Whether this lesson can be delivered over SMS. */
  readonly sms_eligible: boolean
  readonly status: ProgressStatus
  readonly percent: number
}

export interface Module {
  readonly id: number
  readonly title: string
  readonly lessons: readonly LessonSummary[]
}

export interface Progress {
  readonly status: ProgressStatus
  readonly questions_answered: number
  readonly questions_correct: number
  readonly marks_awarded: number
  readonly marks_available: number
  readonly percent: number
  readonly started_at?: string
  readonly completed_at?: string
}

export interface Lesson extends LessonSummary {
  readonly body_markdown: string
  readonly course_id: number
  readonly progress?: Progress
}

export interface Choice {
  readonly label: string
  readonly body: string
}

export interface Question {
  readonly id: number
  readonly kind: string
  readonly prompt: string
  readonly marks: number
  readonly position: number
  readonly choices: readonly Choice[]
  /** What the learner last submitted, if anything. */
  readonly answered_label?: string
}

export interface AssessmentSummary {
  readonly id: number
  readonly title: string
  readonly description: string
  readonly sms_eligible: boolean
  readonly status: ProgressStatus
  readonly percent: number
  readonly marks_awarded: number
  readonly marks_available: number
}

export interface Assessment extends AssessmentSummary {
  readonly questions: readonly Question[]
  readonly progress?: Progress
}

export interface CourseProgress {
  readonly course_id: number
  readonly code: string
  readonly title: string
  readonly lessons_completed: number
  readonly lessons_total: number
  readonly marks_awarded: number
  readonly marks_available: number
  readonly percent: number
}

/** The server's verdict on a submitted answer. */
export interface GradedAnswer {
  readonly question_id: number
  readonly choice_label: string
  readonly correct: boolean
  readonly marks: number
  readonly marks_awarded: number
  readonly marks_total: number
  readonly percent: number
}

export interface IngestResponse {
  readonly event_id: string
  /** True when this exact event was already recorded and nothing changed. */
  readonly duplicate: boolean
  readonly graded?: GradedAnswer
}

/** The error envelope every endpoint returns. */
export interface ApiErrorBody {
  readonly error: string
  readonly code: string
  readonly details?: Record<string, string>
}