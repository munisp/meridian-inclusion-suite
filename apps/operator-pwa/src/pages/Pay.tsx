import { useState } from 'react'
import { loadProfile } from '../profile'
import { presumptiveApi, sha256Hex, naira, Payment, Certificate, PRESUMPTIVE_URL } from '../api'

type Stage = 'idle' | 'intent' | 'authorised' | 'captured' | 'error'

export default function Pay() {
  const profile = loadProfile()
  const [provider, setProvider] = useState('remita')
  const [monthly, setMonthly] = useState(false)
  const [stage, setStage] = useState<Stage>('idle')
  const [payment, setPayment] = useState<Payment | null>(null)
  const [cert, setCert] = useState<Certificate | null>(null)
  const [gate, setGate] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function checkGate() {
    try {
      const resp = await presumptiveApi.get('/v1/gates')
      const g = resp.data.gates?.['G8.presumptive_reg']
      setGate(g ? (g.open ? 'Collections OPEN' : 'Collections CLOSED (awaiting regulation)') : 'Gate unknown')
    } catch {
      setGate('Gate status unavailable (offline)')
    }
  }

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

  return (
    <div className="space-y-4">
      <div className="card">
        <div className="flex items-center justify-between">
          <h2 className="font-bold text-sand-800">Pay presumptive levy</h2>
          <button className="btn-ghost text-xs" onClick={checkGate}>Gate status</button>
        </div>
        {gate && <p className="text-xs mt-1 text-stone-600">{gate}</p>}
        <div className="mt-3 space-y-3">
          <div>
            <label className="label">Payment provider</label>
            <select className="input" value={provider} onChange={(e) => setProvider(e.target.value)}>
              <option value="remita">Remita</option>
              <option value="etranzact">eTranzact</option>
              <option value="flutterwave">Flutterwave</option>
            </select>
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={monthly} onChange={(e) => setMonthly(e.target.checked)} />
            Pay monthly instalment instead of annual
          </label>
          <button className="btn-primary w-full" disabled={busy || !profile} onClick={pay}>
            {busy ? 'Processing…' : 'Pay now'}
          </button>
          {!profile && <p className="text-xs text-amber-800">Save your profile first.</p>}
        </div>
      </div>
      {payment && (
        <div className="card">
          <h3 className="text-sm font-semibold text-sand-800">Payment {payment.id.slice(0, 14)}…</h3>
          <p className="text-lg font-bold">{naira(payment.amount_kobo)}</p>
          <p className="text-xs text-stone-500">
            {payment.provider} · {payment.turnover_band} band · period {payment.period} · status{' '}
            <span className="font-semibold">{payment.status}</span>
          </p>
          <p className="text-xs text-stone-400 mt-1">stage: {stage}</p>
        </div>
      )}
      {cert && (
        <div className="card border-green-800/30 space-y-1">
          <p className="font-bold text-green-800">Payment certificate issued</p>
          <p className="font-mono text-sm">{cert.serial}</p>
          <p className="text-sm">{cert.state} · {cert.band} · {naira(cert.amount_kobo)} · {cert.period}</p>
          <p className="text-xs text-stone-500 break-all">signature: {cert.signature.slice(0, 40)}…</p>
          <a
            className="text-sand-700 underline text-sm"
            href={`${PRESUMPTIVE_URL}/v1/certificates/verify/${cert.serial}`}
            target="_blank"
            rel="noreferrer"
          >
            Verify this certificate publicly
          </a>
        </div>
      )}
      {error && (
        <div className="card border-red-800/30">
          <p className="text-sm text-red-800">{error}</p>
        </div>
      )}
    </div>
  )
}
