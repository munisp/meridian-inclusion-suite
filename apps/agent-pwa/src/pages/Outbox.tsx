import { useCallback, useEffect, useState } from 'react'
import { queuedItems, markSynced, markFailed } from '../db'
import { syncBatch } from '../api'

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
      const batchId = localStorage.getItem('agent.batchId') || 'batch-default'
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
          <h2 className="font-bold text-sand-800">Outbox</h2>
          <p className="text-xs text-stone-500">{online ? 'Online' : 'Offline — records will queue'} · {queued.length} queued · {synced.length} synced</p>
        </div>
        <button className="btn-primary" disabled={busy || !queued.length} onClick={doSync}>
          {busy ? 'Syncing…' : 'Sync now'}
        </button>
      </div>
      {queued.map((r) => (
        <div key={r.id} className="card">
          <p className="font-medium">{r.item.full_name}</p>
          <p className="text-xs text-stone-500">
            NIN {r.item.nin.slice(0, 3)}***** · captured {new Date(r.item.captured_at).toLocaleString()}
          </p>
          {r.error && <p className="text-xs text-red-700">{r.error}</p>}
        </div>
      ))}
      {!queued.length && <p className="text-sm text-stone-500 text-center py-6">Outbox empty — everything synced.</p>}
      {!!log.length && (
        <div className="card">
          <h3 className="text-sm font-semibold text-sand-800 mb-2">Sync log</h3>
          <ul className="text-xs text-stone-600 space-y-1">{log.map((l, i) => <li key={i}>{l}</li>)}</ul>
        </div>
      )}
    </div>
  )
}
