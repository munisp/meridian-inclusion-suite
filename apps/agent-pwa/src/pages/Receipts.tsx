import { useEffect, useState } from 'react'
import { saveReceipt, listReceipts, OfflineReceipt } from '../db'
import { naira } from '../api'

// HMAC-style offline signature: SHA-256(device key | payload). Verified
// server-side on sync (cash_receipt_offline audit trail).
async function sign(payload: string): Promise<string> {
  const key = localStorage.getItem('agent.deviceKey') || (() => {
    const k = crypto.randomUUID()
    localStorage.setItem('agent.deviceKey', k)
    return k
  })()
  const data = new TextEncoder().encode(key + '|' + payload)
  const digest = await crypto.subtle.digest('SHA-256', data)
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('')
}

export default function Receipts() {
  const [payer, setPayer] = useState('')
  const [amount, setAmount] = useState('')
  const [purpose, setPurpose] = useState('presumptive levy')
  const [receipts, setReceipts] = useState<OfflineReceipt[]>([])
  const [issued, setIssued] = useState<OfflineReceipt | null>(null)

  useEffect(() => { listReceipts().then(setReceipts) }, [])

  async function issue(e: React.FormEvent) {
    e.preventDefault()
    const kobo = Math.round(parseFloat(amount.replace(/,/g, '')) * 100)
    if (!payer.trim() || !kobo || kobo <= 0) return
    const serial = 'RCPT-OFF-' + Date.now().toString(36).toUpperCase() + '-' + Math.floor(Math.random() * 1296).toString(36).toUpperCase().padStart(2, '0')
    const agentId = localStorage.getItem('agent.id') || 'agent-demo-1'
    const issuedAt = new Date().toISOString()
    const signature = await sign([serial, payer.trim(), String(kobo), purpose, issuedAt].join('|'))
    const r: OfflineReceipt = { serial, kind: 'cash_receipt_offline', payerName: payer.trim(), amountKobo: kobo, purpose, agentId, issuedAt, signature, synced: false }
    await saveReceipt(r)
    setIssued(r)
    setReceipts(await listReceipts())
    setPayer(''); setAmount('')
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-sand-800 mb-3">Offline cash receipt</h2>
        <form onSubmit={issue} className="space-y-3">
          <div>
            <label className="label">Payer name</label>
            <input className="input" value={payer} onChange={(e) => setPayer(e.target.value)} />
          </div>
          <div>
            <label className="label">Amount (₦)</label>
            <input className="input" inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="15000.00" />
          </div>
          <div>
            <label className="label">Purpose</label>
            <input className="input" value={purpose} onChange={(e) => setPurpose(e.target.value)} />
          </div>
          <button className="btn-primary w-full" type="submit">Issue receipt (offline-signed)</button>
        </form>
      </div>
      {issued && (
        <div className="card border-sand-400">
          <h3 className="font-bold text-sand-800">Receipt issued</h3>
          <p className="font-mono text-sm mt-1">{issued.serial}</p>
          <p className="text-sm">{issued.payerName} — {naira(issued.amountKobo)} ({issued.purpose})</p>
          <p className="text-xs text-stone-500 mt-2 break-all">sig: {issued.signature.slice(0, 32)}…</p>
        </div>
      )}
      <div className="card">
        <h3 className="text-sm font-semibold text-sand-800 mb-2">Issued receipts ({receipts.length})</h3>
        <ul className="divide-y divide-sand-100">
          {receipts.map((r) => (
            <li key={r.serial} className="py-2 text-sm flex justify-between">
              <span>{r.payerName}</span>
              <span className="font-mono text-xs">{naira(r.amountKobo)} · {r.serial.slice(-6)}</span>
            </li>
          ))}
          {!receipts.length && <li className="py-2 text-sm text-stone-500">No receipts yet.</li>}
        </ul>
      </div>
    </div>
  )
}
