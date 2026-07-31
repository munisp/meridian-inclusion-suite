import { ReactNode, useId } from 'react'

interface FieldProps {
  label: string
  children: ReactNode | ((id: string, describedBy?: string, invalid?: boolean) => ReactNode)
  error?: string | null
  hint?: string
  required?: boolean
  /** Override the auto-generated control id (children receive it via render prop). */
  id?: string
}

/**
 * Meridian One §5 — mandatory form-field pattern. Auto-wires
 * id / htmlFor / aria-describedby / aria-invalid so labels are always
 * programmatically associated (audit A2) and errors announce via role="alert"
 * (audit A6).
 *
 * Pass the control as a render child: <Field label="NIN">{id => <input id={id} …/>}</Field>
 */
export default function Field({ label, children, error, hint, required, id }: FieldProps) {
  const autoId = useId()
  const controlId = id ?? autoId
  const hintId = `${controlId}-hint`
  const errorId = `${controlId}-error`
  const describedBy = [error ? errorId : null, hint ? hintId : null].filter(Boolean).join(' ') || undefined
  return (
    <div>
      <label className="label" htmlFor={controlId}>
        {label}
        {required && (
          <span aria-hidden="true" className="text-danger-strong">
            {' '}
            *
          </span>
        )}
      </label>
      {typeof children === 'function'
        ? (children as (id: string, describedBy?: string, invalid?: boolean) => ReactNode)(controlId, describedBy, !!error)
        : children}
      {hint && (
        <p id={hintId} className="text-xs text-stone-600 mt-1">
          {hint}
        </p>
      )}
      {error && (
        <p id={errorId} role="alert" className="text-xs text-danger-strong mt-1 flex items-center gap-1">
          <svg aria-hidden="true" viewBox="0 0 24 24" className="h-3.5 w-3.5 shrink-0" fill="none" stroke="currentColor" strokeWidth="2">
            <circle cx="12" cy="12" r="10" />
            <path d="M12 8v4M12 16h.01" />
          </svg>
          {error}
        </p>
      )}
    </div>
  )
}
