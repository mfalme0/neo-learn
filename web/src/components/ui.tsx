/**
 * Small UI primitives and the design tokens they share.
 *
 * Colours are chosen for WCAG AA contrast against their backgrounds and are
 * never the sole carrier of meaning: status is always carried by a word as
 * well as a colour, because a learner may be reading this on a cheap screen
 * in bright sunlight.
 */

import type { ReactNode } from 'react'
import type { ProgressStatus } from '@/lib/types'

/** The single teal used for actions, tested at 5.2:1 on white. */
const ACCENT = '#0d5c63'
const ACCENT_HOVER = '#0a474d'

export function Button({
  children,
  onClick,
  type = 'button',
  disabled = false,
  variant = 'primary',
  fullWidth = false,
  ariaLabel,
}: {
  children: ReactNode
  onClick?: () => void
  type?: 'button' | 'submit'
  disabled?: boolean
  variant?: 'primary' | 'secondary' | 'ghost'
  fullWidth?: boolean
  ariaLabel?: string
}) {
  const base =
    'inline-flex items-center justify-center gap-2 rounded-md px-4 py-2.5 text-sm font-semibold transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 disabled:cursor-not-allowed disabled:opacity-60'

  const styles: Record<string, string> = {
    primary: `text-white hover:brightness-110 focus-visible:outline-[${ACCENT}]`,
    secondary: 'border border-slate-300 bg-white text-slate-800 hover:bg-slate-50 focus-visible:outline-slate-500',
    ghost: 'text-slate-600 hover:bg-slate-100 hover:text-slate-900 focus-visible:outline-slate-500',
  }

  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      aria-label={ariaLabel}
      aria-busy={disabled || undefined}
      className={`${base} ${styles[variant]} ${fullWidth ? 'w-full' : ''}`}
      style={variant === 'primary' ? { backgroundColor: ACCENT } : undefined}
      onMouseEnter={
        variant === 'primary'
          ? (event) => {
              event.currentTarget.style.backgroundColor = ACCENT_HOVER
            }
          : undefined
      }
      onMouseLeave={
        variant === 'primary'
          ? (event) => {
              event.currentTarget.style.backgroundColor = ACCENT
            }
          : undefined
      }
    >
      {children}
    </button>
  )
}

export function Field({
  label,
  htmlFor,
  type = 'text',
  value,
  onChange,
  error,
  autoComplete,
  required = true,
  inputMode,
}: {
  label: string
  htmlFor: string
  type?: 'text' | 'email' | 'password'
  value: string
  onChange: (value: string) => void
  error?: string
  autoComplete?: string
  required?: boolean
  inputMode?: 'email' | 'text'
}) {
  const errorId = `${htmlFor}-error`

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-sm font-medium text-slate-700">
        {label}
      </label>
      <input
        id={htmlFor}
        name={htmlFor}
        type={type}
        value={value}
        required={required}
        inputMode={inputMode}
        autoComplete={autoComplete}
        aria-invalid={error !== undefined || undefined}
        aria-describedby={error !== undefined ? errorId : undefined}
        onChange={(event) => onChange(event.target.value)}
        className={`w-full rounded-md border px-3 py-2.5 text-base text-slate-900 shadow-sm outline-none focus-visible:ring-2 focus-visible:ring-offset-1 ${
          error !== undefined
            ? 'border-red-600 focus-visible:ring-red-600'
            : 'border-slate-300 focus-visible:ring-slate-500'
        }`}
      />
      {error !== undefined && (
        <p id={errorId} role="alert" className="text-sm text-red-700">
          {error}
        </p>
      )}
    </div>
  )
}

/**
 * Status pill.
 *
 * The text is the message; the colour is decoration. A learner who cannot
 * distinguish the hues still reads "Completed" or "In progress".
 */
export function StatusPill({ status }: { status: ProgressStatus }) {
  const labels: Record<ProgressStatus, string> = {
    not_started: 'Not started',
    in_progress: 'In progress',
    completed: 'Completed',
  }

  const colors: Record<ProgressStatus, string> = {
    not_started: 'bg-slate-100 text-slate-700 ring-slate-300',
    in_progress: 'bg-amber-50 text-amber-900 ring-amber-400',
    completed: 'bg-emerald-50 text-emerald-900 ring-emerald-400',
  }

  return (
    <span
      className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium ring-1 ring-inset ${colors[status]}`}
    >
      {labels[status]}
    </span>
  )
}

/**
 * ProgressBar exposes its value to assistive technology via role="progressbar"
 * with aria-valuenow, because a bare coloured div conveys nothing to a screen
 * reader.
 */
export function ProgressBar({
  percent,
  label,
}: {
  percent: number
  label: string
}) {
  const clamped = Math.max(0, Math.min(100, Math.round(percent)))

  return (
    <div className="flex flex-col gap-1.5">
      <div
        role="progressbar"
        aria-label={label}
        aria-valuenow={clamped}
        aria-valuemin={0}
        aria-valuemax={100}
        className="h-2 w-full overflow-hidden rounded-full bg-slate-200"
      >
        <div
          className="h-full rounded-full bg-emerald-600 transition-[width] duration-500"
          style={{ width: `${clamped}%` }}
        />
      </div>
      <span className="text-xs font-medium text-slate-600">{clamped}%</span>
    </div>
  )
}

export function ErrorMessage({ children }: { children: ReactNode }) {
  return (
    <div
      role="alert"
      className="rounded-md border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-900"
    >
      {children}
    </div>
  )
}

/**
 * EmptyState explains an absence rather than rendering a blank region, so the
 * learner can tell "nothing here yet" from "something failed to load".
 */
export function EmptyState({ title, description }: { title: string; description: string }) {
  return (
    <div className="rounded-lg border border-dashed border-slate-300 px-6 py-12 text-center">
      <h2 className="text-base font-semibold text-slate-800">{title}</h2>
      <p className="mt-1 text-sm text-slate-600">{description}</p>
    </div>
  )
}

export function Skeleton({ className = '' }: { className?: string }) {
  return <div aria-hidden="true" className={`animate-pulse rounded bg-slate-200 ${className}`} />
}

/**
 * LoadingRegion announces itself to screen readers.
 *
 * role="status" with polite live semantics means a screen reader announces
 * "Loading" once, when it appears, rather than interrupting with each update.
 */
export function LoadingRegion({ label = 'Loading' }: { label?: string }) {
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-3">
      <span className="sr-only">{label}</span>
      <Skeleton className="h-6 w-48" />
      <Skeleton className="h-20 w-full" />
      <Skeleton className="h-20 w-full" />
    </div>
  )
}