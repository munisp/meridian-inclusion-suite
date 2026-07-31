import { useEffect, useState } from 'react'
import { ReceiptText } from 'lucide-react'
import { saveReceipt, listReceipts, OfflineReceipt } from '../db'
import { naira, enrollDevice } from '../api'
import Field from '../components/Field'
import Chip from '../components/Chip'
import Empty from '../components/Empty'
import MoneyInput from '../components/MoneyInput'

// Device signing key, generated once per device and ENROLLED server-side
// (audit fix #6): the server binds it to (agent_id, device_id) so offline
// receipts are verifiable via POST /v1/receipts/verify.
export function deviceKey(): string {
  const k = localStorage.getItem('agent.deviceKey')
  if (k) return k
  const nk = crypto.randomUUID() + crypto.randomUUID()
  localStorage.setItem('agent.deviceKey', nk)
  return nk
}

function deviceId(): string {
  const d = localStorage.getItem('agent.deviceId')
  if (d) return d
  const nd = 'dev-' + crypto.randomUUID()
  localStorage.setItem('agent.deviceId', nd)
  return nd
}

// HMAC-style offline signature: SHA-256(device key | payload). Verified
// server-side against the enrolled device key.
async function sign(payload: string): Promise<string> {
  const key = deviceKey()
  const data = new TextEncoder().encode(key + '|' + payload)
  const digest = await crypto.subtle.digest('SHA-256', data)
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('')
}

export default function Receipts() {
  const [payer, setPayer] = useState('')
  const [amountKobo, setAmountKobo] = useState<number | null>(null)
  const [purpose, setPurpose] = useState('presumptive levy')
  const [receipts, setReceipts] = useState<OfflineReceipt[]>([])
  const [issued, setIssued] = useState<OfflineReceipt | null>(null)
  const [formError, setFormError] = useState<string | null>(null)

  const [enrolled, setEnrolled] = useState<boolean | null>(null)

  useEffect(() => {
    listReceipts().then(setReceipts)
    // Enrol this device's key with the server (best effort offline; retried
    // on every mount until it succeeds).
    const agentId = localStorage.getItem('agent.id') || 'agent-demo-1'
    enrollDevice(agentId, deviceId(), deviceKey())
      .then(() => setEnrolled(true))
      .catch(() => setEnrolled(false))
  }, [])

  async function issue(e: React.FormEvent) {
    e.preventDefault()
    setFormError(null)
    if (!payer.trim()) return setFormError('Payer name is required')
    if (!amountKobo || amountKobo <= 0) return setFormError('Enter a valid amount greater than zero')
    const kobo = amountKobo
    const serial = 'RCPT-OFF-' + Date.now().toString(36).toUpperCase() + '-' + Math.floor(Math.random() * 1296).toString(36).toUpperCase().padStart(2, '0')
    const agentId = localStorage.getItem('agent.id') || 'agent-demo-1'
    const issuedAt = new Date().toISOString()
    const signature = await sign([serial, payer.trim(), String(kobo), purpose, issuedAt].join('|'))
    const r: OfflineReceipt = { serial, kind: 'cash_receipt_offline', payerName: payer.trim(), amountKobo: kobo, purpose, agentId, issuedAt, signature, synced: false }
    await saveReceipt(r)
    setIssued(r)
    setReceipts(await listReceipts())
    setPayer(''); setAmountKobo(null)
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-3">Offline cash receipt</h2>
        {enrolled === false && (
          <div className="mb-2 flex items-start gap-2" role="status">
            <Chip status="warning">pending</Chip>
            <p className="text-xs text-warning-on">Device key not yet enrolled with the server — receipts will verify once enrolment succeeds (retried automatically when online).</p>
          </div>
        )}
        {enrolled && (
          <div className="mb-2 flex items-center gap-2" role="status">
            <Chip status="verified">enrolled</Chip>
            <p className="text-xs text-success-on">Device enrolled — receipts are server-verifiable.</p>
          </div>
        )}
        <form onSubmit={issue} className="space-y-3">
          <Field label="Payer name" required>
            {(id, describedBy) => (
              <input id={id} aria-describedby={describedBy} aria-required="true" className="input" value={payer} onChange={(e) => setPayer(e.target.value)} />
            )}
          </Field>
          <Field label="Amount" required>
            {(id, describedBy) => (
              <MoneyInput id={id} aria-describedby={describedBy} aria-required={true} valueKobo={amountKobo} onChangeKobo={setAmountKobo} />
            )}
          </Field>
          <Field label="Purpose">
            {(id, describedBy) => (
              <input id={id} aria-describedby={describedBy} className="input" value={purpose} onChange={(e) => setPurpose(e.target.value)} />
            )}
          </Field>
          {formError && (
            <p role="alert" className="text-sm text-danger-strong">{formError}</p>
          )}
          <button className="btn-primary w-full" type="submit">Issue receipt (offline-signed)</button>
        </form>
      </div>
      {issued && (
        <div className="card border-success-strong/30" aria-live="polite">
          <h3 className="font-bold text-neutral-800">Receipt issued</h3>
          <p className="font-mono text-sm mt-1">{issued.serial}</p>
          <p className="text-sm">{issued.payerName} — {naira(issued.amountKobo)} ({issued.purpose})</p>
          <p className="text-xs text-stone-600 mt-2 break-all">sig: {issued.signature.slice(0, 32)}…</p>
        </div>
      )}
      {receipts.length ? (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800 mb-2">Issued receipts ({receipts.length})</h3>
          <ul className="divide-y divide-neutral-100">
            {receipts.map((r) => (
              <li key={r.serial} className="py-2 text-sm flex justify-between">
                <span>{r.payerName}</span>
                <span className="font-mono text-xs tabular-nums">{naira(r.amountKobo)} · {r.serial.slice(-6)}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <Empty icon={ReceiptText} title="No receipts yet" body="Offline-signed receipts you issue appear here." />
      )}
    </div>
  )
}
