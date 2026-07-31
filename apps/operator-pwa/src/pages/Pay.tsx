import { useEffect, useState } from 'react'
import { Copy, Check, ExternalLink } from 'lucide-react'
import { loadProfile } from '../profile'
import { presumptiveApi, sha256Hex, naira, Payment, Certificate, PRESUMPTIVE_URL } from '../api'
import Field from '../components/Field'
import Chip from '../components/Chip'

type Stage = 'idle' | 'intent' | 'authorised' | 'captured' | 'error'
type Gate = { open: boolean } | 'unknown' | 'unavailable' | null

// Meridian One §6 — wizard-lite: the intent→authorise→capture sequence is
// three network hops; on 2G each can take seconds, so every stage gets
// explicit progress feedback (audit O2).
const STEPS: { stage: Stage; label: string }[] = [
  { stage: 'intent', label: 'Contacting provider' },
  { stage: 'authorised', label: 'Authorising payment' },
  { stage: 'captured', label: 'Issuing certificate' },
]

function stepIndex(stage: Stage): number {
  if (stage === 'captured') return 3
  const i = STEPS.findIndex((s) => s.stage === stage)
  return i < 0 ? -1 : i
}

export default function Pay() {
  const profile = loadProfile()
  const [provider, setProvider] = useState('remita')
  const [monthly, setMonthly] = useState(false)
  const [stage, setStage] = useState<Stage>('idle')
  const [payment, setPayment] = useState<Payment | null>(null)
  const [cert, setCert] = useState<Certificate | null>(null)
  const [gate, setGate] = useState<Gate>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState(false)

  // O3: gate is checked proactively on mount; a closed gate blocks Pay.
  useEffect(() => {
    presumptiveApi
      .get('/v1/gates')
      .then((resp) => {
        const g = resp.data.gates?.['G8.presumptive_reg']
        setGate(g ? { open: !!g.open } : 'unknown')
      })
      .catch(() => setGate('unavailable'))
  }, [])

  const gateOpen = gate !== null && typeof gate === 'object' && gate.open
  const gateClosed = gate !== null && typeof gate === 'object' && !gate.open

  async function pay() {
    if (!profile || !profile.tin) return setError('Save your profile with a TIN first')
    setBusy(true); setError(null)
    try {
      const tinHash = await sha256Hex('dev:' + profile.tin) // client-side pseudonym for the demo flow
      const intent = await presumptiveApi.post<Payment>('/v1/payments/intent', {
        tin_hash: tinHash,
        state: profile.state,
        trade_category: profile.tradeCategory,
        annual_turnover_kobo: profile.annualTurnoverKobo,
        provider,
        monthly,
      })
      setPayment(intent.data)
      setStage('intent')
      const auth = await presumptiveApi.post(`/v1/payments/${intent.data.id}/authorise`)
      setPayment(auth.data.payment)
      if (auth.data.payment.status !== 'authorised') {
        setStage('error')
        setError('Authorisation failed: ' + (auth.data.authorisation?.detail || 'unknown'))
        return
      }
      setStage('authorised')
      const cap = await presumptiveApi.post(`/v1/payments/${intent.data.id}/capture`)
      setPayment(cap.data.payment)
      setCert(cap.data.certificate)
      setStage('captured')
    } catch (e: any) {
      setStage('error')
      const detail = e?.response?.data?.detail || e?.message || String(e)
      setError(detail)
    } finally {
      setBusy(false)
    }
  }

  async function copySerial() {
    if (!cert) return
    try {
      await navigator.clipboard.writeText(cert.serial)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch { /* clipboard unavailable */ }
  }

  const current = stepIndex(stage)

  return (
    <div className="space-y-4">
      <div className="card">
        <div className="flex items-center justify-between gap-2">
          <h2 className="font-bold text-neutral-800">Pay presumptive levy</h2>
          {gateClosed && <Chip status="warning">Collections closed</Chip>}
          {gateOpen && <Chip status="success">Collections open</Chip>}
          {gate === 'unavailable' && <Chip status="info">Gate status offline</Chip>}
        </div>
        {gateClosed && (
          <p className="text-xs text-warning-on mt-1">Collections are closed pending regulation — payment is disabled for now.</p>
        )}
        <div className="mt-3 space-y-3">
          <Field label="Payment provider">
            {(id, describedBy) => (
              <select id={id} aria-describedby={describedBy} className="input" value={provider} onChange={(e) => setProvider(e.target.value)}>
                <option value="remita">Remita</option>
                <option value="etranzact">eTranzact</option>
                <option value="flutterwave">Flutterwave</option>
              </select>
            )}
          </Field>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={monthly} onChange={(e) => setMonthly(e.target.checked)} />
            Pay monthly instalment instead of annual
          </label>
          <button className="btn-primary w-full" disabled={busy || !profile || gateClosed} onClick={pay}>
            {busy ? 'Processing…' : 'Pay now'}
          </button>
          {!profile && (
            <p className="text-xs" role="status">
              <Chip status="warning">Save your profile first.</Chip>
            </p>
          )}
        </div>
      </div>
      {busy && current >= 0 && (
        <div className="card" aria-live="polite">
          <p className="text-sm font-medium mb-2">
            Step {Math.min(current + 1, 3)} of 3 — {STEPS[Math.min(current, 2)].label}…
          </p>
          <ol className="space-y-1">
            {STEPS.map((s, i) => (
              <li key={s.stage} className={`flex items-center gap-2 text-xs ${i < current ? 'text-success-on' : i === current ? 'text-info-on font-semibold' : 'text-stone-600'}`}>
                <span
                  aria-hidden="true"
                  className={`h-4 w-4 rounded-full border flex items-center justify-center ${i < current ? 'bg-success-strong border-success-strong text-white' : i === current ? 'border-info-strong animate-pulse' : 'border-neutral-300'}`}
                >
                  {i < current ? '✓' : i + 1}
                </span>
                {s.label}
              </li>
            ))}
          </ol>
          <p className="text-xs text-stone-600 mt-2">This can take a moment on slow connections — keep the app open.</p>
        </div>
      )}
      {payment && (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800">Payment {payment.id.slice(0, 14)}…</h3>
          <p className="text-lg font-bold tabular-nums">{naira(payment.amount_kobo)}</p>
          <p className="text-xs text-stone-600 flex items-center gap-2 flex-wrap">
            {payment.provider} · {payment.turnover_band} band · period {payment.period}{' '}
            <Chip status={chipFor(payment.status)}>{payment.status}</Chip>
          </p>
        </div>
      )}
      {cert && (
        <div className="card border-success-strong/30 space-y-1" aria-live="polite">
          <p className="font-bold text-success-on">Payment certificate issued</p>
          <p className="font-mono text-sm flex items-center gap-2 flex-wrap">
            <code>{cert.serial}</code>
            <button
              type="button"
              onClick={copySerial}
              aria-label="Copy certificate serial"
              className="inline-flex items-center gap-1 text-xs text-brand-700 underline focus-visible:ring-2 focus-visible:ring-brand-700 rounded"
            >
              {copied ? <Check aria-hidden="true" className="h-3.5 w-3.5" /> : <Copy aria-hidden="true" className="h-3.5 w-3.5" />}
              {copied ? 'Copied' : 'Copy'}
            </button>
          </p>
          <p className="text-sm">{cert.state} · {cert.band} · {naira(cert.amount_kobo)} · {cert.period}</p>
          <p className="text-xs text-stone-600 break-all">signature: {cert.signature.slice(0, 40)}…</p>
          <a
            className="inline-flex items-center gap-1 text-brand-700 underline text-sm focus-visible:ring-2 focus-visible:ring-brand-700 rounded"
            href={`${PRESUMPTIVE_URL}/v1/certificates/verify/${cert.serial}`}
            target="_blank"
            rel="noreferrer"
          >
            Verify this certificate publicly
            <ExternalLink aria-hidden="true" className="h-3.5 w-3.5" />
            <span className="sr-only">(opens in a new tab)</span>
          </a>
        </div>
      )}
      {error && (
        <div className="card border-danger-strong/30" role="alert">
          <p className="text-sm text-danger-strong">{error}</p>
        </div>
      )}
    </div>
  )
}

function chipFor(status: string): 'paid' | 'pending' | 'failed' | 'captured' {
  const s = status.toLowerCase()
  if (['captured', 'paid', 'authorised'].includes(s)) return 'paid'
  if (s.includes('fail') || s.includes('reject')) return 'failed'
  if (s.includes('pend') || s.includes('intent')) return 'pending'
  return 'captured'
}
