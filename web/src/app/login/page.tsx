'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { ApiError, api } from '@/lib/api'
import { Button, ErrorMessage, Field } from '@/components/ui'

/**
 * Sign-in form.
 *
 * Field-level errors are rendered next to their inputs and referenced with
 * aria-describedby; the summary error is a live region so a screen reader
 * announces a failure without the user having to hunt for it.
 */
export default function LoginPage() {
  const router = useRouter()

  const [institution, setInstitution] = useState('kibera-secondary')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()

    const errors: Record<string, string> = {}
    if (institution.trim() === '') {
      errors.institution = 'Enter your school or institution.'
    }
    if (email.trim() === '') {
      errors.email = 'Enter your email address.'
    }
    if (password === '') {
      errors.password = 'Enter your password.'
    }
    if (Object.keys(errors).length > 0) {
      setFieldErrors(errors)
      setFormError('Please correct the highlighted fields.')
      return
    }

    setFieldErrors({})
    setFormError(null)
    setSubmitting(true)

    try {
      const user = await api.login(institution.trim(), email.trim(), password)
      // A teacher has no learner interface yet, so sending them to the
      // dashboard would show an empty page with no explanation.
      router.replace(user.role === 'teacher' ? '/coming-soon' : '/')
    } catch (error: unknown) {
      if (error instanceof ApiError) {
        if (error.status === 401) {
          setFormError('That institution, email, or password is not correct.')
        } else if (error.isNetworkError) {
          setFormError(
            'Could not reach the Neo Learn server. Check your connection and try again.',
          )
        } else {
          setFieldErrors(error.details)
          setFormError(error.message)
        }
      } else {
        setFormError('Something went wrong. Please try again.')
      }
      setSubmitting(false)
    }
  }

  return (
    <main id="main" className="mx-auto flex min-h-screen max-w-md flex-col justify-center px-4 py-12">
      <div className="mb-8 text-center">
        <h1 className="text-2xl font-bold tracking-tight text-slate-900">Neo Learn</h1>
        <p className="mt-1 text-sm text-slate-600">
          Sign in to continue your lessons
        </p>
      </div>

      <form onSubmit={handleSubmit} noValidate className="flex flex-col gap-5">
        {formError !== null && <ErrorMessage>{formError}</ErrorMessage>}

        <Field
          label="School or institution"
          htmlFor="institution"
          value={institution}
          onChange={setInstitution}
          autoComplete="organization"
          error={fieldErrors.institution}
        />
        <Field
          label="Email address"
          htmlFor="email"
          type="email"
          inputMode="email"
          value={email}
          onChange={setEmail}
          autoComplete="username"
          error={fieldErrors.email}
        />
        <Field
          label="Password"
          htmlFor="password"
          type="password"
          value={password}
          onChange={setPassword}
          autoComplete="current-password"
          error={fieldErrors.password}
        />

        <Button type="submit" disabled={submitting} fullWidth>
          {submitting ? 'Signing in...' : 'Sign in'}
        </Button>
      </form>

      <p className="mt-8 text-center text-xs text-slate-500">
        Lessons and assessments stay available over SMS if you lose
        connection.
      </p>
    </main>
  )
}