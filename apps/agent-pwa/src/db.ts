import { openDB, DBSchema, IDBPDatabase } from 'idb'
import type { CaptureItem } from './api'

interface OutboxRecord {
  id?: number
  batchId: string
  item: CaptureItem
  status: 'queued' | 'synced' | 'failed'
  error?: string
  createdAt: string
}

interface AgentDB extends DBSchema {
  outbox: { key: number; value: OutboxRecord; indexes: { 'by-status': string; 'by-batch': string } }
  receipts: { key: string; value: OfflineReceipt }
}

export interface OfflineReceipt {
  serial: string
  kind: 'cash_receipt_offline'
  payerName: string
  amountKobo: number
  purpose: string
  agentId: string
  issuedAt: string
  signature: string
  synced: boolean
}

let dbp: Promise<IDBPDatabase<AgentDB>> | null = null

export function db(): Promise<IDBPDatabase<AgentDB>> {
  if (!dbp) {
    dbp = openDB<AgentDB>('meridian-agent', 1, {
      upgrade(d) {
        const outbox = d.createObjectStore('outbox', { keyPath: 'id', autoIncrement: true })
        outbox.createIndex('by-status', 'status')
        outbox.createIndex('by-batch', 'batchId')
        d.createObjectStore('receipts', { keyPath: 'serial' })
      },
    })
  }
  return dbp
}

export async function queueCapture(batchId: string, item: CaptureItem): Promise<void> {
  await (await db()).add('outbox', { batchId, item, status: 'queued', createdAt: new Date().toISOString() })
}

export async function queuedItems(status: 'queued' | 'synced' | 'failed' | 'all' = 'queued'): Promise<OutboxRecord[]> {
  const d = await db()
  if (status === 'all') return d.getAll('outbox')
  return d.getAllFromIndex('outbox', 'by-status', status)
}

export async function markSynced(ids: number[]): Promise<void> {
  const d = await db()
  const tx = d.transaction('outbox', 'readwrite')
  for (const id of ids) {
    const rec = await tx.store.get(id)
    if (rec) await tx.store.put({ ...rec, status: 'synced' })
  }
  await tx.done
}

export async function markFailed(ids: number[], error: string): Promise<void> {
  const d = await db()
  const tx = d.transaction('outbox', 'readwrite')
  for (const id of ids) {
    const rec = await tx.store.get(id)
    if (rec) await tx.store.put({ ...rec, status: 'failed', error })
  }
  await tx.done
}

export async function saveReceipt(r: OfflineReceipt): Promise<void> {
  await (await db()).put('receipts', r)
}

export async function listReceipts(): Promise<OfflineReceipt[]> {
  return (await db()).getAll('receipts')
}

export function newBatchId(): string {
  return 'batch-' + crypto.randomUUID()
}

export function newClientRef(): string {
  return crypto.randomUUID()
}
