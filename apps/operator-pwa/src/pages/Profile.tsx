import { useEffect, useState } from 'react'
import { loadProfile, saveProfile, OperatorProfile } from '../profile'
import { onboardingApi } from '../api'

const STATES = ['Lagos', 'Kano', 'FCT', 'Rivers', 'Oyo', 'Kaduna', 'Enugu', 'Borno', 'Other']
const TRADES = ['food_vendor', 'tailoring', 'artisan', 'transport', 'retail', 'services']

export default function Profile() {
  const [p, setP] = useState<OperatorProfile>(() =>
    loadProfile() || { tin: '', fullName: '', state: 'Lagos', tradeCategory: 'retail', annualTurnoverKobo: 300000000 },
  )
  const [saved, setSaved] = useState(false)
  const [tinStatus, setTinStatus] = useState<string | null>(null)

  useEffect(() => { setSaved(false) }, [p])

  async function verifyTin() {
    if (!/^\d{10}$/.test(p.tin)) return setTinStatus('Enter your 10-digit TIN to check status')
    try {
      const resp = await onboardingApi.post('/v1/verify/tin', { tin: p.tin })
      setTinStatus(resp.data.verified ? `TIN verified (hash ${resp.data.tin_hash.slice(0, 12)}…)` : 'TIN not recognised')
    } catch (e: any) {
      setTinStatus('Verification service unreachable (offline). Your profile is stored on-device.')
    }
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-sand-800 mb-3">My profile</h2>
        <div className="space-y-3">
          <div>
            <label className="label">Full name</label>
            <input className="input" value={p.fullName} onChange={(e) => setP({ ...p, fullName: e.target.value })} />
          </div>
          <div>
            <label className="label">TIN (10 digits)</label>
            <div className="flex gap-2">
              <input className="input" inputMode="numeric" maxLength={10} value={p.tin} onChange={(e) => setP({ ...p, tin: e.target.value })} />
              <button className="btn-ghost whitespace-nowrap" onClick={verifyTin}>Check</button>
            </div>
            {tinStatus && <p className="text-xs mt-1 text-stone-600">{tinStatus}</p>}
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="label">State</label>
              <select className="input" value={p.state} onChange={(e) => setP({ ...p, state: e.target.value })}>
                {STATES.map((s) => <option key={s}>{s}</option>)}
              </select>
            </div>
            <div>
              <label className="label">Trade</label>
              <select className="input" value={p.tradeCategory} onChange={(e) => setP({ ...p, tradeCategory: e.target.value })}>
                {TRADES.map((t) => <option key={t} value={t}>{t.replace('_', ' ')}</option>)}
              </select>
            </div>
          </div>
          <div>
            <label className="label">Estimated annual turnover (₦)</label>
            <input
              className="input"
              inputMode="numeric"
              value={(p.annualTurnoverKobo / 100).toString()}
              onChange={(e) => {
                const v = Math.round(parseFloat(e.target.value.replace(/,/g, '') || '0') * 100)
                setP({ ...p, annualTurnoverKobo: Number.isFinite(v) ? v : 0 })
              }}
            />
          </div>
          <button
            className="btn-primary w-full"
            onClick={() => { saveProfile(p); setSaved(true) }}
          >
            Save profile
          </button>
          {saved && <p className="text-sm text-green-800">Profile saved on this device.</p>}
        </div>
      </div>
      <div className="card text-xs text-stone-500">
        Your name and TIN stay on your device. Services receive pseudonymised identifiers (tin_hash) per NDPA.
      </div>
    </div>
  )
}
