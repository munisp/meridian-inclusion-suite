import { ReactNode } from 'react'
import { Inbox, LucideIcon } from 'lucide-react'

interface EmptyProps {
  title: string
  body?: string
  action?: ReactNode
  icon?: LucideIcon
}

/** Meridian One §5 — illustration-free empty state (bytes matter). */
export default function Empty({ title, body, action, icon: Icon = Inbox }: EmptyProps) {
  return (
    <div className="card flex flex-col items-center text-center py-8 px-4">
      <span className="rounded-full bg-neutral-100 p-3 mb-3">
        <Icon aria-hidden="true" className="h-6 w-6 text-neutral-500" />
      </span>
      <p className="text-lg font-semibold">{title}</p>
      {body && <p className="text-sm text-stone-600 mt-1">{body}</p>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  )
}
