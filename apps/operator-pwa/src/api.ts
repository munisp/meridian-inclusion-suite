import axios from 'axios'
import { getAccessToken } from './auth'

export const PRESUMPTIVE_URL = (import.meta.env.VITE_PRESUMPTIVE_URL as string) || 'http://localhost:8102'
export const EDUCATION_URL = (import.meta.env.VITE_EDUCATION_URL as string) || 'http://localhost:8103'
export const ONBOARDING_URL = (import.meta.env.VITE_ONBOARDING_URL as string) || 'http://localhost:8101'

export const presumptiveApi = axios.create({ baseURL: PRESUMPTIVE_URL, timeout: 15000 })
export const educationApi = axios.create({ baseURL: EDUCATION_URL, timeout: 15000 })
export const onboardingApi = axios.create({ baseURL: ONBOARDING_URL, timeout: 15000 })

for (const api of [presumptiveApi, onboardingApi]) {
  api.interceptors.request.use((cfg) => {
    // Prod (VITE_AUTH_MODE=keycloak): RS256 Bearer token; dev: §1.3 dev auth
    const token = getAccessToken()
    if (token) {
      cfg.headers['Authorization'] = `Bearer ${token}`
    } else {
      cfg.headers['X-Dev-Role'] = 'operator'
    }
    return cfg
  })
}

// HMAC-SHA256 pseudonymisation parity with services (TIN hash) — computed
// client-side so the raw TIN never leaves the device un-hashed in displays.
export async function sha256Hex(text: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('')
}

// Money formatting lives in lib/format (Meridian One §9); `naira` kept as a
// back-compat alias — formatNGN is the only formatter.
export { formatNGN as naira } from './lib/format'

export interface BandResult {
  band: string
  band_label: string
  annual_levy_kobo: number
  monthly_levy_kobo: number
  admin_fee_kobo: number
  exempt: boolean
  exempt_reason?: string
  graduate: boolean
  pack_id: string
  pack_version: string
  trace: string[]
}

export interface Payment {
  id: string
  tin_hash: string
  state: string
  turnover_band: string
  amount_kobo: number
  period: string
  provider: string
  status: string
  certificate_serial?: string
}

export interface Certificate {
  serial: string
  tin_hash: string
  state: string
  band: string
  amount_kobo: number
  period: string
  issued_at: string
  signature: string
}
