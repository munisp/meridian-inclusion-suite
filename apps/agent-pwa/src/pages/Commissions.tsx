import { useEffect, useState } from 'react'
import { UserCheck } from 'lucide-react'
import { fetchOperators, fetchFloatBalance, fetchCommissionSummary, naira, CommissionSummary } from '../api'
import Chip, { chipStatusFor } from '../components/Chip'
import Empty from '../components/Empty'

function SkeletonRows({ n = 3 }: { n?: number }) {
  return (
    <ul aria-busy="true" aria-label="Loading" className="divide-y divide-neutral-100 animate-pulse">
      {Array.from({ length: n }).map((_, i) => (
        <li key={i} className="py-2 flex justify-between">
          <span className="h-4 w-32 rounded bg-neutral-100" />
          <span className="h-4 w-16 rounded-full bg-neutral-100" />
        </li>
      ))}
    </ul>
  )
}

export default function Commissions() {
  const [rows, setRows] = useState<any[]>([])
  const [float, setFloat] = useState<any>(null)
  const [summary, setSummary] = useState<CommissionSummary | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const storedAgentId = localStorage.getItem('agent.id')
  const agentId = storedAgentId || 'agent-demo-1'
  const isDemo = !storedAgentId

  useEffect(() => {
    setLoading(true)
    fetchOperators()
      .then(setRows)
      .catch((e) => setError('Onboarding service unreachable: ' + (e?.message || e)))
      .finally(() => setLoading(false))
    fetchFloatBalance(agentId).then(setFloat).catch(() => setFloat(null))
    // audit fix #2: commissions are computed SERVER-SIDE, keyed to the
    // authenticated agent identity (JWT sub in prod). The PWA never computes
    // the amount and never asserts the identity itself.
    fetchCommissionSummary().then(setSummary).catch(() => setSummary(null))
  }, [agentId])

  const mine = rows.filter((r) => r.agent_id === agentId)
  const verified = summary ? summary.verified : mine.filter((r) => ['nin_verified', 'tin_provisioned', 'graduated'].includes(r.status)).length
  const accrued = summary ? summary.accrued_kobo : 0

  return (
    <div className="space-y-4">
      {isDemo && (
        <div className="card flex items-center gap-2" role="status">
          <Chip status="demo">DEMO</Chip>
          <p className="text-xs text-stone-600">No enrolled agent identity on this device — showing demo data.</p>
        </div>
      )}
      <div className="card">
        <h2 className="font-bold text-neutral-800">Commission dashboard</h2>
        <p className="text-xs text-stone-600">Agent: {summary?.agent_id || agentId} · amounts computed server-side {summary ? `(rate ${naira(summary.rate_kobo)}, ${summary.rule_pack_version})` : '(offline — awaiting server summary)'}</p>
      </div>
      {error && (
        <div className="card flex items-center gap-2" role="alert">
          <Chip status="warning">offline</Chip>
          <p className="text-sm text-warning-on">{error} — showing offline placeholders.</p>
        </div>
      )}
      <div className="grid grid-cols-3 gap-3" aria-live="polite">
        <div className="card text-center">
          <p className="text-2xl font-bold text-brand-700 tabular-nums">{summary ? summary.captured : mine.length}</p>
          <p className="text-xs text-stone-600">Captured</p>
        </div>
        <div className="card text-center">
          <p className="text-2xl font-bold text-brand-700 tabular-nums">{verified}</p>
          <p className="text-xs text-stone-600">Verified</p>
        </div>
        <div className="card text-center">
          <p className="text-2xl font-bold text-brand-700 tabular-nums">{naira(accrued)}</p>
          <p className="text-xs text-stone-600">Accrued</p>
        </div>
      </div>
      {float && (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800">Float (ledger 100)</h3>
          <p className="text-lg font-bold tabular-nums">{naira(float.credits_posted - float.debits_posted)}</p>
          <p className="text-xs text-stone-600">
            topped up {naira(float.credits_posted)} · drawn {naira(float.debits_posted)}
          </p>
        </div>
      )}
      {loading ? (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800 mb-2">Recent captures</h3>
          <SkeletonRows />
        </div>
      ) : mine.length ? (
        <div className="card">
          <h3 className="text-sm font-semibold text-neutral-800 mb-2">Recent captures</h3>
          <ul className="divide-y divide-neutral-100">
            {mine.slice(0, 20).map((r) => (
              <li key={r.id} className="py-2 flex justify-between items-center text-sm">
                <span>{r.full_name}</span>
                <Chip status={chipStatusFor(r.status)}>{r.status}</Chip>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <Empty icon={UserCheck} title="No captures yet" body="Start with the Capture tab — records appear here after sync." />
      )}
    </div>
  )
}
