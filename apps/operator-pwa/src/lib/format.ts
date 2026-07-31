// Meridian One §9 — Nigerian-context formatting utilities.
// Money is kobo integers end-to-end; never float-parse money inputs directly
// (use parseNGNToKobo / MoneyInput).

export function formatNGN(kobo: number): string {
  return '₦' + (kobo / 100).toLocaleString('en-NG', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
}

/** ₦1.2m-style compaction for dashboard stat cards only — never receipts. */
export function formatNGNCompact(kobo: number): string {
  const naira = kobo / 100
  const abs = Math.abs(naira)
  if (abs >= 1_000_000) return '₦' + (naira / 1_000_000).toLocaleString('en-NG', { maximumFractionDigits: 1 }) + 'm'
  if (abs >= 1_000) return '₦' + (naira / 1_000).toLocaleString('en-NG', { maximumFractionDigits: 1 }) + 'k'
  return formatNGN(kobo)
}

/**
 * Strips ₦, commas and spaces; rejects decimals >2dp; returns kobo or null
 * on garbage (callers show an inline error via MoneyInput).
 */
export function parseNGNToKobo(input: string): number | null {
  const cleaned = input.replace(/[₦,\s]/g, '')
  if (cleaned === '') return null
  if (!/^\d+(\.\d{1,2})?$/.test(cleaned)) return null
  const kobo = Math.round(Number(cleaned) * 100)
  return Number.isFinite(kobo) ? kobo : null
}

/** Normalise 0803…/234803…/+234… → '+234 803 000 0000' display string. */
export function formatPhoneNG(p: string): string {
  let d = p.replace(/\D/g, '')
  if (d.startsWith('234')) d = d.slice(3)
  else if (d.startsWith('0')) d = d.slice(1)
  if (d.length !== 10) return p // unrecognised; return as entered
  return `+234 ${d.slice(0, 3)} ${d.slice(3, 6)} ${d.slice(6)}`
}

export function formatDateNG(iso: string): string {
  return new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' })
}

export function formatDateTimeNG(iso: string): string {
  return (
    new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' }) +
    ', ' +
    new Date(iso).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' }) +
    ' WAT'
  )
}
