import { useState } from 'react'
import { parseNGNToKobo, formatNGN } from '../lib/format'

interface MoneyInputProps {
  id?: string
  valueKobo: number | null
  onChangeKobo: (kobo: number | null) => void
  placeholder?: string
  invalid?: boolean
  'aria-describedby'?: string
  'aria-required'?: boolean
}

/**
 * Meridian One §9 — the only sanctioned money entry control.
 * inputMode=decimal, live thousand-separators, ₦ prefix adornment, stores
 * kobo; garbage input surfaces an inline validation error instead of a
 * silent 0 (audit A10/O4).
 */
export default function MoneyInput({ id, valueKobo, onChangeKobo, placeholder, invalid, ...aria }: MoneyInputProps) {
  const [raw, setRaw] = useState(valueKobo != null ? (valueKobo / 100).toLocaleString('en-NG') : '')
  const [error, setError] = useState<string | null>(null)
  const errorId = id ? `${id}-money-error` : undefined

  function handle(e: React.ChangeEvent<HTMLInputElement>) {
    const v = e.target.value
    setRaw(v)
    if (v.trim() === '') {
      setError(null)
      onChangeKobo(null)
      return
    }
    const kobo = parseNGNToKobo(v)
    if (kobo == null) {
      setError('Enter a valid amount, e.g. 15,000 or 15000.00')
      onChangeKobo(null)
    } else {
      setError(null)
      onChangeKobo(kobo)
    }
  }

  function normalise() {
    const kobo = parseNGNToKobo(raw)
    if (kobo != null) setRaw((kobo / 100).toLocaleString('en-NG', { minimumFractionDigits: 2, maximumFractionDigits: 2 }))
  }

  return (
    <div>
      <div className="relative">
        <span aria-hidden="true" className="absolute left-3 top-1/2 -translate-y-1/2 text-stone-600">
          ₦
        </span>
        <input
          id={id}
          className={`input pl-7 ${invalid || error ? 'border-danger-strong' : ''}`}
          inputMode="decimal"
          value={raw}
          onChange={handle}
          onBlur={normalise}
          placeholder={placeholder ?? '15,000.00'}
          aria-invalid={invalid || !!error || undefined}
          aria-describedby={[aria['aria-describedby'], error ? errorId : null].filter(Boolean).join(' ') || undefined}
          aria-required={aria['aria-required']}
        />
      </div>
      {error && (
        <p id={errorId} role="alert" className="text-xs text-danger-strong mt-1">
          {error}
        </p>
      )}
      {!error && raw.trim() !== '' && parseNGNToKobo(raw) != null && (
        <p className="text-xs text-stone-600 mt-1">{formatNGN(parseNGNToKobo(raw)!)}</p>
      )}
    </div>
  )
}
