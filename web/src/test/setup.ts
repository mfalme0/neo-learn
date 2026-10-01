import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
import { cleanup } from '@testing-library/react'

/*
 * jsdom implements neither of these, and the progress bar and accordion
 * patterns both depend on them. Stubbing them here rather than per-test keeps
 * the components free of test-only conditionals.
 */
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

if (typeof window !== 'undefined') {
  // jsdom has no matchMedia. Several layout-related code paths probe it.
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  })
}