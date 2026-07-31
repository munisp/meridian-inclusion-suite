import { useState } from 'react'
import { SearchX } from 'lucide-react'
import { educationApi, naira, EDUCATION_URL } from '../api'
import Field from '../components/Field'
import Empty from '../components/Empty'
import MoneyInput from '../components/MoneyInput'

interface CalcResult {
  amount_kobo: number
  amount_naira: number
  provision_citation: string
  disclaimer: string
  trace: string[]
}

export default function Education() {
  const [incomeKobo, setIncomeKobo] = useState<number | null>(300000000)
  const [rentKobo, setRentKobo] = useState<number | null>(0)
  const [pit, setPit] = useState<CalcResult | null>(null)
  const [faqQ, setFaqQ] = useState('')
  const [faqHits, setFaqHits] = useState<any[]>([])
  const [faqSearched, setFaqSearched] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function runPit() {
    setError(null)
    try {
      const resp = await educationApi.post<CalcResult>('/v1/calc/pit', {
        annual_gross_income_kobo: incomeKobo ?? 0,
        annual_rent_paid_kobo: rentKobo ?? 0,
      })
      setPit(resp.data)
    } catch (e: any) {
      setError('Education service unreachable: ' + (e?.message || e))
    }
  }

  async function searchFaq() {
    if (faqQ.trim().length < 2) return
    try {
      const resp = await educationApi.get('/v1/faq/search', { params: { q: faqQ, limit: 3 } })
      setFaqHits(resp.data.results)
    } catch {
      setFaqHits([])
    }
    setFaqSearched(true)
  }

  return (
    <div className="space-y-4">
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-2">PIT calculator (NTA 2025)</h2>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Annual income">
            {(id, describedBy) => (
              <MoneyInput id={id} aria-describedby={describedBy} valueKobo={incomeKobo} onChangeKobo={setIncomeKobo} />
            )}
          </Field>
          <Field label="Annual rent paid">
            {(id, describedBy) => (
              <MoneyInput id={id} aria-describedby={describedBy} valueKobo={rentKobo} onChangeKobo={setRentKobo} />
            )}
          </Field>
        </div>
        <button className="btn-primary w-full mt-3" onClick={runPit}>Calculate</button>
        {error && (
          <p role="alert" className="text-xs text-danger-strong mt-2">{error}</p>
        )}
        {pit && (
          <div className="mt-3 space-y-1" aria-live="polite">
            <p className="text-xl font-bold tabular-nums">{naira(pit.amount_kobo)}</p>
            <p className="text-xs text-stone-600">{pit.provision_citation}</p>
            <details className="text-xs text-stone-600">
              <summary className="cursor-pointer font-medium text-brand-700">Trace</summary>
              <ul className="mt-1 space-y-1 list-disc pl-4">{pit.trace.map((t, i) => <li key={i}>{t}</li>)}</ul>
            </details>
            <p className="text-[11px] text-stone-600">{pit.disclaimer}</p>
          </div>
        )}
      </div>
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-2">Tax FAQ search</h2>
        <Field label="Search questions">
          {(id, describedBy) => (
            <div className="flex gap-2">
              <input
                id={id}
                aria-describedby={describedBy}
                className="input"
                placeholder="e.g. WHT rates"
                value={faqQ}
                onChange={(e) => setFaqQ(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && searchFaq()}
              />
              <button className="btn-ghost" onClick={searchFaq}>Search</button>
            </div>
          )}
        </Field>
        {faqHits.length > 0 && (
          <ul className="mt-2 space-y-2">
            {faqHits.map((h) => (
              <li key={h.id} className="text-sm">
                <p className="font-medium">{h.question}</p>
                <p className="text-xs text-stone-600 mt-0.5">{h.answer}</p>
              </li>
            ))}
          </ul>
        )}
        {faqSearched && faqHits.length === 0 && (
          <div className="mt-2">
            <Empty icon={SearchX} title="No answers found" body="Try different keywords, or ask the tax assistant below." />
          </div>
        )}
      </div>
      <div className="card">
        <h2 className="font-bold text-neutral-800 mb-2">Ask the tax assistant (T14)</h2>
        <iframe
          title="NRS Tax Help"
          src={`${EDUCATION_URL}/embed/chat`}
          className="w-full h-[520px] rounded-lg border border-neutral-200 bg-neutral-50"
          loading="lazy"
        />
      </div>
    </div>
  )
}
