import { useState } from 'react'
import { queueCapture, newBatchId, newClientRef } from '../db'
import { NG_STATES, lgasForState } from '../lib/ng-geo'
import { TRADE_CATEGORIES, tradeLabel } from '../lib/trades'
import Field from '../components/Field'

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
    // DATA-LOSS FIX: a fresh batch id per capture. The previous code persisted
    // one batchId in localStorage forever; the server dedups batches on the
    // Idempotency-Key, so every sync after the first was discarded as a
    // "duplicate" and its items were lost. Record-level dedup is handled by
    // the per-item client_ref, so batch ids must never be reused.
    const batchId = newBatchId()
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

  const lgas = lgasForState(form.state)

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-1">Operator onboarding</h2>
        <p className="text-xs text-stone-600 mb-4">
          Captured records go to the offline outbox and sync when connectivity returns (72h+ tolerant).
        </p>
        <form onSubmit={submit} className="space-y-3">
          <Field label="NIN (11 digits)" required>
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                aria-required="true"
                className="input font-mono"
                inputMode="numeric"
                maxLength={11}
                value={form.nin}
                onChange={set('nin')}
                placeholder="12345678901"
              />
            )}
          </Field>
          <Field label="Full name" required>
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                aria-required="true"
                className="input"
                value={form.full_name}
                onChange={set('full_name')}
                placeholder="Adaeze Okafor"
              />
            )}
          </Field>
          <Field label="Phone">
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                className="input"
                inputMode="tel"
                value={form.phone}
                onChange={set('phone')}
                placeholder="0803 000 0000"
              />
            )}
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="State" required>
              {(id, describedBy) => (
                <select id={id} aria-describedby={describedBy} className="input" value={form.state} onChange={set('state')}>
                  {NG_STATES.map((s) => (
                    <option key={s}>{s}</option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="LGA">
              {(id, describedBy) =>
                lgas.length ? (
                  <select id={id} aria-describedby={describedBy} className="input" value={form.lga} onChange={set('lga')}>
                    <option value="">Select LGA</option>
                    {lgas.map((l) => (
                      <option key={l}>{l}</option>
                    ))}
                  </select>
                ) : (
                  <input id={id} aria-describedby={describedBy} className="input" value={form.lga} onChange={set('lga')} placeholder="Ikeja" />
                )
              }
            </Field>
          </div>
          <Field label="Trade category">
            {(id, describedBy) => (
              <select id={id} aria-describedby={describedBy} className="input" value={form.trade_category} onChange={set('trade_category')}>
                {TRADE_CATEGORIES.map((tr) => (
                  <option key={tr} value={tr}>
                    {tradeLabel(tr)}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <label className="flex items-start gap-2 text-sm text-stone-600">
            <input type="checkbox" className="mt-1" checked={consent} onChange={(e) => setConsent(e.target.checked)} />
            <span>Operator consented to NRS data processing (NDPA) for tax registration purposes.</span>
          </label>
          {error && (
            <p role="alert" className="text-sm text-danger-strong flex items-center gap-1.5">
              <svg aria-hidden="true" viewBox="0 0 24 24" className="h-4 w-4 shrink-0" fill="none" stroke="currentColor" strokeWidth="2">
                <circle cx="12" cy="12" r="10" />
                <path d="M12 8v4M12 16h.01" />
              </svg>
              {error}
            </p>
          )}
          <button className="btn-primary w-full" type="submit">Save to outbox</button>
        </form>
      </div>
      {saved && (
        <div className="card border-success-strong/30" aria-live="polite">
          <p className="text-sm text-success-on font-medium">Queued offline. Sync ref: {saved.slice(0, 8)}…</p>
        </div>
      )}
    </div>
  )
}
