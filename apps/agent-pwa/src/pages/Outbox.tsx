import { useCallback, useEffect, useState } from 'react'
import { CloudOff } from 'lucide-react'
import { queuedItems, markSynced, markFailed } from '../db'
import { syncBatch } from '../api'
import Chip from '../components/Chip'
import Empty from '../components/Empty'

interface Row {
  id?: number
  item: { client_ref: string; full_name: string; nin: string; captured_at: string }
  status: string
  error?: string
}

export default function Outbox() {
  const [queued, setQueued] = useState<Row[]>([])
  const [synced, setSynced] = useState<Row[]>([])
  const [busy, setBusy] = useState(false)
  const [log, setLog] = useState<string[]>([])
  const [online, setOnline] = useState(navigator.onLine)

  const refresh = useCallback(async () => {
    setQueued((await queuedItems('queued')) as Row[])
    setSynced((await queuedItems('synced')) as Row[])
  }, [])

  const doSync = useCallback(async () => {
    const items = (await queuedItems('queued')) as any[]
    if (!items.length) return
    setBusy(true)
    try {
      const agentId = localStorage.getItem('agent.id') || 'agent-demo-1'
      // DATA-LOSS FIX: a NEW Idempotency-Key per sync attempt. Reusing a key
      // makes the server replay the first batch's result and drop the new
      // items. Safe retries of THIS attempt are still deduped by the server
      // via per-item client_ref.
      const batchId = 'batch-' + crypto.randomUUID()
      const result = await syncBatch(agentId, batchId, items.map((r) => r.item))
      const createdOrResolved = new Set(
        result.results.filter((r) => r.outcome !== 'rejected').map((r) => r.client_ref),
      )
      const okIds = items.filter((r) => createdOrResolved.has(r.item.client_ref)).map((r) => r.id!)
      const badIds = items.filter((r) => !createdOrResolved.has(r.item.client_ref)).map((r) => r.id!)
      await markSynced(okIds)
      if (badIds.length) await markFailed(badIds, 'rejected by server')
      setLog((l) => [
        `${new Date().toLocaleTimeString()} batch ${result.id.slice(0, 14)}…: ${result.results
          .map((r) => r.outcome)
          .join(', ')}`,
        ...l,
      ])
    } catch (err: any) {
      setLog((l) => [`${new Date().toLocaleTimeString()} sync failed: ${err?.message || err}`, ...l])
    } finally {
      setBusy(false)
      refresh()
    }
  }, [refresh])

  useEffect(() => {
    refresh()
    const onOnline = () => { setOnline(true); doSync() }
    const onOffline = () => setOnline(false)
    const onSwMsg = (e: MessageEvent) => { if ((e.data as any)?.type === 'OUTBOX_SYNC_REQUESTED') doSync() }
    window.addEventListener('online', onOnline)
    window.addEventListener('offline', onOffline)
    navigator.serviceWorker?.addEventListener('message', onSwMsg)
    return () => {
      window.removeEventListener('online', onOnline)
      window.removeEventListener('offline', onOffline)
      navigator.serviceWorker?.removeEventListener('message', onSwMsg)
    }
  }, [doSync, refresh])

  return (
    <div className="space-y-4">
      <div className="card flex items-center justify-between">
        <div>
          <h2 className="font-bold text-neutral-800">Outbox</h2>
          <p className="text-xs text-stone-600" aria-live="polite">{online ? 'Online' : 'Offline — records will queue'} · {queued.length} queued · {synced.length} synced</p>
        </div>
        <button className="btn-primary" disabled={busy || !queued.length} onClick={doSync}>
          {busy ? 'Syncing…' : 'Sync now'}
        </button>
      </div>
      {queued.map((r) => (
        <div key={r.id} className="card">
          <div className="flex items-center justify-between gap-2">
            <p className="font-medium">{r.item.full_name}</p>
            <Chip status={r.error ? 'failed' : 'queued'}>{r.error ? 'failed' : 'queued'}</Chip>
          </div>
          <p className="text-xs text-stone-600 mt-0.5">
            NIN {r.item.nin.slice(0, 3)}***** · captured {new Date(r.item.captured_at).toLocaleString()}
          </p>
          {r.error && (
            <div className="mt-2 flex items-center justify-between gap-2">
              <p role="alert" className="text-xs text-danger-strong">{r.error}</p>
              <button className="btn-ghost text-xs px-3 py-2" disabled={busy || !online} onClick={doSync}>
                Retry now
              </button>
            </div>
          )}
        </div>
      ))}
      {!queued.length && (
        <Empty icon={CloudOff} title="Outbox empty" body="Everything synced. New captures queue here when you're offline." />
      )}
      {!!log.length && (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800 mb-2">Sync log</h3>
          <ul className="text-xs text-stone-600 space-y-1">{log.map((l, i) => <li key={i}>{l}</li>)}</ul>
        </div>
      )}
    </div>
  )
}
