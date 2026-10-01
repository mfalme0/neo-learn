import Link from 'next/link'
import { EmptyState } from '@/components/ui'

/**
 * Placeholder for the roles and surfaces that exist in the schema but have no
 * interface yet.
 *
 * Shown to teachers and admins rather than an empty dashboard: a learner
 * interface for an educator account is a confusing thing to land on, and
 * quietly omitting it hides that the work exists but is not built.
 */
export default function ComingSoonPage() {
  return (
    <div className="flex max-w-xl flex-col gap-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight text-slate-900">
          Educator interface
        </h1>
        <p className="mt-1 text-sm text-slate-600">
          Authoring courses, monitoring learners, and reviewing results are
          planned for a later milestone.
        </p>
      </div>

      <EmptyState
        title="Not built yet"
        description="The data model supports courses, lessons, assessments, and per-tenant teacher roles already. What is missing is the interface over them."
      />

      <Link href="/" className="text-sm font-medium text-slate-700 underline">
        Back to my learning
      </Link>
    </div>
  )
}