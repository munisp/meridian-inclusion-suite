import { useEffect, useState } from 'react'
import { fetchOperators, fetchFloatBalance, naira } from '../api'

const COMMISSION_PER_VERIFIED_KOBO = 20000 // ₦200 per NIN/TIN-verified operator

export default function Commissions() {
  const [rows, setRows] = useState<any[]>([])
  const [float, setFloat] = useState<any>(null)
  const [error, setError] = useState<string | null>(null)
  const agentId = localStorage.getItem('agent.id') || 'agent-demo-1'

  useEffect(() => {
    fetchOperators()
      .then(setRows)
      .catch((e) => setError('Onboarding service unreachable: ' + (e?.message || e)))
    fetchFloatBalance(agentId).then(setFloat).catch(() => setFloat(null))
  }, [agentId])

  const mine = rows.filter((r) => r.agent_id === agentId)
  const verified = mine.filter((r) => ['nin_verified', 'tin_provisioned', 'graduated'].includes(r.status))
  const accrued = verified.length * COMMISSION_PER_VERIFIED_KOBO

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-sand-800">Commission dashboard</h2>
        <p className="text-xs text-stone-500">Agent: {agentId} (set localStorage agent.id to switch)</p>
      </div>
      {error && <div className="card text-sm text-amber-800">{error} — showing offline placeholders.</div>}
      <div className="grid grid-cols-3 gap-3">
        <div className="card text-center">
          <p className="text-2xl font-bold text-sand-700">{mine.length}</p>
          <p className="text-xs text-stone-500">Captured</p>
        </div>
        <div className="card text-center">
          <p className="text-2xl font-bold text-sand-700">{verified.length}</p>
          <p className="text-xs text-stone-500">Verified</p>
        </div>
        <div className="card text-center">
          <p className="text-2xl font-bold text-sand-700">{naira(accrued)}</p>
          <p className="text-xs text-stone-500">Accrued</p>
        </div>
      </div>
      {float && (
        <div className="card">
          <h3 className="text-sm font-semibold text-sand-800">Float (ledger 100)</h3>
          <p className="text-lg font-bold">{naira(float.credits_posted - float.debits_posted)}</p>
          <p className="text-xs text-stone-500">
            topped up {naira(float.credits_posted)} · drawn {naira(float.debits_posted)}
          </p>
        </div>
      )}
      <div className="card">
        <h3 className="text-sm font-semibold text-sand-800 mb-2">Recent captures</h3>
        <ul className="divide-y divide-sand-100">
          {mine.slice(0, 20).map((r) => (
            <li key={r.id} className="py-2 flex justify-between text-sm">
              <span>{r.full_name}</span>
              <span className="text-xs rounded-full px-2 py-0.5 bg-sand-100 text-sand-700">{r.status}</span>
            </li>
          ))}
          {!mine.length && <li className="py-2 text-sm text-stone-500">No captures attributed yet.</li>}
        </ul>
      </div>
    </div>
  )
}
