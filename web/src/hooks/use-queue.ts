'use client'

import { useEffect, useState } from 'react'
import { offlineQueue, type OfflineQueue, type QueueSnapshot } from '@/lib/offline-queue'

/**
 * Re-renders when the offline queue changes, so the banner reflects a flush
 * that started or finished outside this component's own event handlers.
 *
 * The queue is a parameter defaulting to the singleton so tests can supply
 * their own instance: the singleton stays locked if any other test leaves a
 * request in flight.
 */
export function useQueue(queue: OfflineQueue = offlineQueue): QueueSnapshot {
  const [snapshot, setSnapshot] = useState<QueueSnapshot>(() => queue.snapshot())

  useEffect(() => queue.subscribe(setSnapshot), [queue])

  return snapshot
}

/** Tracks browser connectivity, which is what the SMS fallback exists for. */
export function useOnline(): boolean {
  const [online, setOnline] = useState(true)

  useEffect(() => {
    // navigator.onLine is only a hint; it reports "connected to a network",
    // not "the server is reachable". The queue treats a real failure as the
    // authority.
    setOnline(navigator.onLine)

    const goOnline = () => setOnline(true)
    const goOffline = () => setOnline(false)

    window.addEventListener('online', goOnline)
    window.addEventListener('offline', goOffline)
    return () => {
      window.removeEventListener('online', goOnline)
      window.removeEventListener('offline', goOffline)
    }
  }, [])

  return online
}