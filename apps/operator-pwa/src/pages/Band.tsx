import { useState } from 'react'
import { loadProfile } from '../profile'
import { presumptiveApi, naira, BandResult } from '../api'

export default function Band() {
  const profile = loadProfile()
  const [result, setResult] = useState<BandResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function evaluate() {
    if (!profile) return setError('Save your profile first')
    setBusy(true); setError(null)
    try {
      const resp = await presumptiveApi.post<BandResult>('/v1/bands/evaluate', {
        state: profile.state,
        trade_category: profile.tradeCategory,
        annual_turnover_kobo: profile.annualTurnoverKobo,
      })
      setResult(resp.data)
    } catch (e: any) {
      setError('Presumptive service unreachable: ' + (e?.message || e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-sand-800">My presumptive band</h2>
        {profile ? (
          <p className="text-sm text-stone-600 mt-1">
            {profile.state} · {profile.tradeCategory.replace('_', ' ')} · turnover {naira(profile.annualTurnoverKobo)}
          </p>
        ) : (
          <p className="text-sm text-amber-800 mt-1">Save your profile first.</p>
        )}
        <button className="btn-primary w-full mt-3" disabled={busy || !profile} onClick={evaluate}>
          {busy ? 'Evaluating…' : 'Evaluate my band'}
        </button>
        {error && <p className="text-sm text-red-700 mt-2">{error}</p>}
      </div>
      {result && (
        <div className="card space-y-2">
          {result.exempt ? (
            <>
              <p className="text-lg font-bold text-green-800">Exempt</p>
              <p className="text-sm">{result.exempt_reason}</p>
            </>
          ) : result.graduate ? (
            <p className="text-sm">Your turnover is above the presumptive ceiling — you graduate to the standard regime (MBS). The NRS will contact you about full registration.</p>
          ) : (
            <>
              <p className="text-lg font-bold text-sand-800">{result.band_label}</p>
              <p className="text-2xl font-bold">{naira(result.annual_levy_kobo)}<span className="text-sm font-normal text-stone-500"> / year</span></p>
              <p className="text-sm text-stone-600">
                or {naira(result.monthly_levy_kobo)} / month · admin fee {naira(result.admin_fee_kobo)}
              </p>
              <p className="text-xs text-stone-500">Rule pack: {result.pack_id}@{result.pack_version}</p>
              <details className="text-xs text-stone-600">
                <summary className="cursor-pointer font-medium text-sand-700">Calculation trace</summary>
                <ul className="mt-1 space-y-1 list-disc pl-4">
                  {result.trace.map((t, i) => <li key={i}>{t}</li>)}
                </ul>
              </details>
            </>
          )}
        </div>
      )}
    </div>
  )
}
