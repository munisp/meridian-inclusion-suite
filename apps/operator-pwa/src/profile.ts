// Operator profile stored locally (device-first; the server stores only
// pseudonymised nin_hash/tin_hash in events).
export interface OperatorProfile {
  tin: string
  fullName: string
  state: string
  tradeCategory: string
  annualTurnoverKobo: number
}

const KEY = 'operator.profile'

export function loadProfile(): OperatorProfile | null {
  try {
    const raw = localStorage.getItem(KEY)
    return raw ? (JSON.parse(raw) as OperatorProfile) : null
  } catch {
    return null
  }
}

export function saveProfile(p: OperatorProfile): void {
  localStorage.setItem(KEY, JSON.stringify(p))
}
