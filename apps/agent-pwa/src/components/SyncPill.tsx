import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

/**
 * Meridian One §5 — offline-first signature component: persistent header
 * pill showing Online / Offline — n queued / Syncing. aria-live polite so
 * status changes are announced.
 */
export default function SyncPill({ queued = 0, syncing = false }: { queued?: number; syncing?: boolean }) {
  const { t } = useTranslation('common')
  const [online, setOnline] = useState(navigator.onLine)
  useEffect(() => {
    const on = () => setOnline(true)
    const off = () => setOnline(false)
    window.addEventListener('online', on)
    window.addEventListener('offline', off)
    return () => {
      window.removeEventListener('online', on)
      window.removeEventListener('offline', off)
    }
  }, [])

  const state = syncing ? 'syncing' : online ? 'online' : 'offline'
  const dot =
    state === 'online' ? 'bg-success-strong' : state === 'offline' ? 'bg-warning-strong' : 'bg-info-strong animate-pulse'
  const label =
    state === 'online'
      ? t('sync.online')
      : state === 'syncing'
        ? t('sync.syncing')
        : t('sync.offlineQueued', { count: queued })

  return (
    <span
      role="status"
      aria-live="polite"
      className="inline-flex items-center gap-1.5 rounded-full bg-white/10 px-2.5 py-1 text-xs font-medium text-white"
    >
      <span aria-hidden="true" className={`h-2 w-2 rounded-full ${dot}`} />
      {label}
    </span>
  )
}
