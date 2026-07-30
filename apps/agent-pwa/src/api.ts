import axios from 'axios'

// Service base URLs (env-configurable; dev defaults per README port map)
export const ONBOARDING_URL = (import.meta.env.VITE_ONBOARDING_URL as string) || 'http://localhost:8101'
export const PRESUMPTIVE_URL = (import.meta.env.VITE_PRESUMPTIVE_URL as string) || 'http://localhost:8102'

export const onboardingApi = axios.create({ baseURL: ONBOARDING_URL, timeout: 15000 })
export const presumptiveApi = axios.create({ baseURL: PRESUMPTIVE_URL, timeout: 15000 })

for (const api of [onboardingApi, presumptiveApi]) {
  api.interceptors.request.use((cfg) => {
    // §1.3 dev auth
    cfg.headers['X-Dev-Role'] = 'operator'
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
