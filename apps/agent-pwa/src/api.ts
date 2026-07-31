import axios from 'axios'
import { getAccessToken } from './auth'

// Service base URLs (env-configurable; dev defaults per README port map)
export const ONBOARDING_URL = (import.meta.env.VITE_ONBOARDING_URL as string) || 'http://localhost:8101'
export const PRESUMPTIVE_URL = (import.meta.env.VITE_PRESUMPTIVE_URL as string) || 'http://localhost:8102'

export const onboardingApi = axios.create({ baseURL: ONBOARDING_URL, timeout: 15000 })
export const presumptiveApi = axios.create({ baseURL: PRESUMPTIVE_URL, timeout: 15000 })

for (const api of [onboardingApi, presumptiveApi]) {
  api.interceptors.request.use((cfg) => {
    // Prod (VITE_AUTH_MODE=keycloak): RS256 Bearer token; dev: §1.3 dev auth
    const token = getAccessToken()
    if (token) {
      cfg.headers['Authorization'] = `Bearer ${token}`
    } else {
      // AUTH_MODE=dev only: the server honours these dev stand-ins solely
      // outside profile=prod; prod identity comes from the JWT sub.
      cfg.headers['X-Dev-Role'] = 'operator'
      cfg.headers['X-Dev-Agent-Id'] = localStorage.getItem('agent.id') || 'agent-demo-1'
    }
    return cfg
  })
}

export interface CaptureItem {
  client_ref: string
  nin: string
  full_name: string
  phone: string
  state: string
  lga: string
  trade_category: string
  captured_at: string
}

export interface CaptureBatchResult {
  id: string
  idempotency_key: string
  status: string
  results: { client_ref: string; operator_id?: string; outcome: string; detail?: string }[]
}

export async function syncBatch(agentId: string, idempotencyKey: string, items: CaptureItem[]): Promise<CaptureBatchResult> {
  const resp = await onboardingApi.post<CaptureBatchResult>(
    '/v1/capture/batch',
    { agent_id: agentId, items },
    { headers: { 'Idempotency-Key': idempotencyKey } },
  )
  return resp.data
}

export async function fetchOperators(): Promise<any[]> {
  const resp = await onboardingApi.get('/v1/operators')
  return resp.data.operators || []
}

export async function fetchFloatBalance(agentId: string): Promise<any> {
  const resp = await presumptiveApi.get(`/v1/float/${agentId}/balance`)
  return resp.data
}

export const naira = (kobo: number) =>
  '₦' + (kobo / 100).toLocaleString('en-NG', { minimumFractionDigits: 2, maximumFractionDigits: 2 })

// Device enrolment (audit fix #6): registers this device's signing key with
// the server so offline receipts become server-verifiable.
export async function enrollDevice(agentId: string, deviceId: string, key: string): Promise<void> {
  await presumptiveApi.post('/v1/devices/enroll', { agent_id: agentId, device_id: deviceId, key })
}

export interface CommissionSummary {
  agent_id: string
  captured: number
  verified: number
  accrued_kobo: number
  rate_kobo: number
  rule_pack_version: string
}

// Server-side commission summary (audit fix #2): computed by the onboarding
// service from the registry, keyed to the authenticated agent identity.
export async function fetchCommissionSummary(): Promise<CommissionSummary> {
  const resp = await onboardingApi.get<CommissionSummary>('/v1/commissions/summary')
  return resp.data
}
