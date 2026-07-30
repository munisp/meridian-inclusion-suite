import { useState } from 'react'
import { queueCapture, newBatchId, newClientRef } from '../db'

const STATES = ['Lagos', 'Kano', 'FCT', 'Rivers', 'Oyo', 'Kaduna', 'Enugu', 'Borno', 'Other']
const TRADES = ['food_vendor', 'tailoring', 'artisan', 'transport', 'retail', 'services']

export default function Capture() {
  const [form, setForm] = useState({ nin: '', full_name: '', phone: '', state: 'Lagos', lga: '', trade_category: 'retail' })
  const [saved, setSaved] = useState<string | null>(null)
  const [consent, setConsent] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm({ ...form, [k]: e.target.value })

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)
    if (!/^\d{11}$/.test(form.nin)) return setError('NIN must be exactly 11 digits')
    if (form.full_name.trim().length < 3) return setError('Full name is required')
    if (!consent) return setError('Operator consent (NDPA) must be recorded')
    const batchId = localStorage.getItem('agent.batchId') || newBatchId()
    localStorage.setItem('agent.batchId', batchId)
    const ref = newClientRef()
    await queueCapture(batchId, {
      client_ref: ref,
      nin: form.nin,
      full_name: form.full_name.trim(),
      phone: form.phone.trim(),
      state: form.state,
      lga: form.lga.trim(),
      trade_category: form.trade_category,
      captured_at: new Date().toISOString(),
    })
    setSaved(ref)
    setForm({ nin: '', full_name: '', phone: '', state: form.state, lga: '', trade_category: form.trade_category })
    setConsent(false)
    if ('serviceWorker' in navigator && 'SyncManager' in window) {
      const reg = await navigator.serviceWorker.ready
      try { await (reg as any).sync.register('outbox-sync') } catch { /* best effort */ }
    }
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-sand-800 mb-1">Operator onboarding</h2>
        <p className="text-xs text-stone-500 mb-4">
          Captured records go to the offline outbox and sync when connectivity returns (72h+ tolerant).
        </p>
        <form onSubmit={submit} className="space-y-3">
          <div>
            <label className="label">NIN (11 digits)</label>
            <input className="input" inputMode="numeric" maxLength={11} value={form.nin} onChange={set('nin')} placeholder="12345678901" />
          </div>
          <div>
            <label className="label">Full name</label>
            <input className="input" value={form.full_name} onChange={set('full_name')} placeholder="Adaeze Okafor" />
          </div>
          <div>
            <label className="label">Phone</label>
            <input className="input" inputMode="tel" value={form.phone} onChange={set('phone')} placeholder="0803 000 0000" />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="label">State</label>
              <select className="input" value={form.state} onChange={set('state')}>
                {STATES.map((s) => <option key={s}>{s}</option>)}
              </select>
            </div>
            <div>
              <label className="label">LGA</label>
              <input className="input" value={form.lga} onChange={set('lga')} placeholder="Ikeja" />
            </div>
          </div>
          <div>
            <label className="label">Trade category</label>
            <select className="input" value={form.trade_category} onChange={set('trade_category')}>
              {TRADES.map((t) => <option key={t} value={t}>{t.replace('_', ' ')}</option>)}
            </select>
          </div>
          <label className="flex items-start gap-2 text-sm text-stone-600">
            <input type="checkbox" className="mt-1" checked={consent} onChange={(e) => setConsent(e.target.checked)} />
            <span>Operator consented to NRS data processing (NDPA) for tax registration purposes.</span>
          </label>
          {error && <p className="text-sm text-red-700">{error}</p>}
          <button className="btn-primary w-full" type="submit">Save to outbox</button>
        </form>
      </div>
      {saved && (
        <div className="card border-green-800/30">
          <p className="text-sm text-green-800 font-medium">Queued offline. Sync ref: {saved.slice(0, 8)}…</p>
        </div>
      )}
    </div>
  )
}
