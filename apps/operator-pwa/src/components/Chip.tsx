import { ReactNode } from 'react'

export type ChipStatus =
  | 'captured'
  | 'queued'
  | 'pending'
  | 'verified'
  | 'synced'
  | 'paid'
  | 'failed'
  | 'rejected'
  | 'demo'
  | 'success'
  | 'warning'
  | 'danger'
  | 'info'

// Meridian One §5 — status is always a chip (semantic surface + on-surface
// text + icon), never coloured text alone.
const STATUS_STYLES: Record<ChipStatus, { cls: string; icon: string }> = {
  captured: { cls: 'bg-info text-info-on', icon: '●' },
  queued: { cls: 'bg-warning text-warning-on', icon: '◷' },
  pending: { cls: 'bg-warning text-warning-on', icon: '◷' },
  verified: { cls: 'bg-success text-success-on', icon: '✓' },
  synced: { cls: 'bg-success text-success-on', icon: '✓' },
  paid: { cls: 'bg-success text-success-on', icon: '✓' },
  failed: { cls: 'bg-danger text-danger-on', icon: '✕' },
  rejected: { cls: 'bg-danger text-danger-on', icon: '✕' },
  demo: { cls: 'bg-neutral-100 text-neutral-800', icon: '◌' },
  success: { cls: 'bg-success text-success-on', icon: '✓' },
  warning: { cls: 'bg-warning text-warning-on', icon: '⚠' },
  danger: { cls: 'bg-danger text-danger-on', icon: '✕' },
  info: { cls: 'bg-info text-info-on', icon: 'ℹ' },
}

/** Map an arbitrary server status string to a canonical chip status. */
export function chipStatusFor(status: string): ChipStatus {
  const s = status.toLowerCase()
  if (s in STATUS_STYLES) return s as ChipStatus
  if (['nin_verified', 'tin_provisioned', 'graduated', 'authorised', 'created', 'resolved'].includes(s)) return 'verified'
  if (s.includes('fail') || s.includes('reject') || s.includes('error')) return 'failed'
  if (s.includes('pend') || s.includes('queue') || s.includes('await')) return 'pending'
  return 'captured'
}

interface ChipProps {
  status: ChipStatus
  children?: ReactNode
  className?: string
}

export default function Chip({ status, children, className = '' }: ChipProps) {
  const s = STATUS_STYLES[status]
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium ${s.cls} ${className}`}
    >
      <span aria-hidden="true">{s.icon}</span>
      {children ?? status}
    </span>
  )
}
