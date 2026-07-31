import { useEffect, useState } from 'react'
import { loadProfile, saveProfile, OperatorProfile } from '../profile'
import { onboardingApi } from '../api'
import { NG_STATES } from '../lib/ng-geo'
import { TRADE_CATEGORIES, tradeLabel } from '../lib/trades'
import Field from '../components/Field'
import Chip from '../components/Chip'
import MoneyInput from '../components/MoneyInput'

type TinStatus = { kind: 'success' | 'warning' | 'info'; text: string } | null

export default function Profile() {
  const [p, setP] = useState<OperatorProfile>(() =>
    loadProfile() || { tin: '', fullName: '', state: 'Lagos', tradeCategory: 'retail', annualTurnoverKobo: 300000000 },
  )
  const [saved, setSaved] = useState(false)
  const [tinStatus, setTinStatus] = useState<TinStatus>(null)

  useEffect(() => { setSaved(false) }, [p])

  async function verifyTin() {
    if (!/^\d{10}$/.test(p.tin)) return setTinStatus({ kind: 'info', text: 'Enter your 10-digit TIN to check status' })
    try {
      const resp = await onboardingApi.post('/v1/verify/tin', { tin: p.tin })
      setTinStatus(
        resp.data.verified
          ? { kind: 'success', text: `TIN verified (hash ${resp.data.tin_hash.slice(0, 12)}…)` }
          : { kind: 'warning', text: 'TIN not recognised' },
      )
    } catch (e: any) {
      setTinStatus({ kind: 'warning', text: 'Verification service unreachable (offline). Your profile is stored on-device.' })
    }
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-3">My profile</h2>
        <div className="space-y-3">
          <Field label="Full name" required>
            {(id, describedBy) => (
              <input id={id} aria-describedby={describedBy} className="input" value={p.fullName} onChange={(e) => setP({ ...p, fullName: e.target.value })} />
            )}
          </Field>
          <Field label="TIN (10 digits)" required>
            {(id, describedBy) => (
              <div>
                <div className="flex gap-2">
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    className="input font-mono"
                    inputMode="numeric"
                    maxLength={10}
                    value={p.tin}
                    onChange={(e) => setP({ ...p, tin: e.target.value })}
                  />
                  <button className="btn-ghost whitespace-nowrap" onClick={verifyTin}>Check</button>
                </div>
                {tinStatus && (
                  <p className="mt-1" role="status">
                    <Chip status={tinStatus.kind}>{tinStatus.text}</Chip>
                  </p>
                )}
              </div>
            )}
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="State" required>
              {(id, describedBy) => (
                <select id={id} aria-describedby={describedBy} className="input" value={p.state} onChange={(e) => setP({ ...p, state: e.target.value })}>
                  {NG_STATES.map((s) => (
                    <option key={s}>{s}</option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="Trade">
              {(id, describedBy) => (
                <select id={id} aria-describedby={describedBy} className="input" value={p.tradeCategory} onChange={(e) => setP({ ...p, tradeCategory: e.target.value })}>
                  {TRADE_CATEGORIES.map((tr) => (
                    <option key={tr} value={tr}>
                      {tradeLabel(tr)}
                    </option>
                  ))}
                </select>
              )}
            </Field>
          </div>
          <Field label="Estimated annual turnover" hint="Used to place you in a presumptive band.">
            {(id, describedBy) => (
              <MoneyInput
                id={id}
                aria-describedby={describedBy}
                valueKobo={p.annualTurnoverKobo}
                onChangeKobo={(kobo) => setP({ ...p, annualTurnoverKobo: kobo ?? 0 })}
              />
            )}
          </Field>
          <button
            className="btn-primary w-full"
            onClick={() => { saveProfile(p); setSaved(true) }}
          >
            Save profile
          </button>
          {saved && (
            <p className="text-sm" role="status">
              <Chip status="success">Profile saved on this device.</Chip>
            </p>
          )}
        </div>
      </div>
      <div className="card text-xs text-stone-600">
        Your name and TIN stay on your device. Services receive pseudonymised identifiers (tin_hash) per NDPA.
      </div>
    </div>
  )
}
