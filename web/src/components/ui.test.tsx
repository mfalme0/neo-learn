import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ProgressBar, StatusPill } from './ui'

/**
 * The accessibility assertions here are not incidental. A learner using a
 * screen reader on a cheap Android handset is one of the exact users this
 * product exists for, and a progress bar rendered as a bare coloured div is
 * invisible to them.
 */

describe('StatusPill', () => {
  it.each([
    ['not_started', 'Not started'],
    ['in_progress', 'In progress'],
    ['completed', 'Completed'],
  ] as const)('renders %s as the words "%s"', (status, label) => {
    render(<StatusPill status={status} />)
    expect(screen.getByText(label)).toBeInTheDocument()
  })

  it('carries the status in text, not only in colour', () => {
    // A learner who cannot distinguish the hues must still be able to read
    // the state.
    render(<StatusPill status="completed" />)
    expect(screen.getByText('Completed').textContent).toBe('Completed')
  })
})

describe('ProgressBar', () => {
  it('exposes the value to assistive technology', () => {
    render(<ProgressBar percent={42} label="Mathematics overall progress" />)

    const bar = screen.getByRole('progressbar', { name: 'Mathematics overall progress' })
    expect(bar).toHaveAttribute('aria-valuenow', '42')
    expect(bar).toHaveAttribute('aria-valuemin', '0')
    expect(bar).toHaveAttribute('aria-valuemax', '100')
  })

  it('also shows the value visually', () => {
    render(<ProgressBar percent={42} label="Progress" />)
    expect(screen.getByText('42%')).toBeInTheDocument()
  })

  it.each([
    [0, '0%'],
    [100, '100%'],
    [33.4, '33%'],
    [33.6, '34%'],
  ])('renders %s as "%s"', (percent, expected) => {
    render(<ProgressBar percent={percent} label="Progress" />)
    expect(screen.getByText(expected)).toBeInTheDocument()
  })

  it('clamps a value outside 0-100 rather than rendering an invalid bar', () => {
    // A corrupted projection must not produce aria-valuenow="150", which
    // violates the ARIA range contract.
    render(<ProgressBar percent={150} label="Progress" />)
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '100')

    screen.getByText('100%')
    expect(screen.queryByText('150%')).not.toBeInTheDocument()
  })

  it('clamps a negative value to zero', () => {
    render(<ProgressBar percent={-20} label="Progress" />)
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '0')
  })
})