// Shared presumptive-tax trade taxonomy (used by capture + profile forms).
export const TRADE_CATEGORIES = ['food_vendor', 'tailoring', 'artisan', 'transport', 'retail', 'services'] as const

export function tradeLabel(t: string): string {
  return t.replace(/_/g, ' ')
}
