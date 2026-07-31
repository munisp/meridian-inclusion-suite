import { useEffect, useState } from 'react'
import { NavLink, Route, Routes } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Plus, ArrowDownUp, Banknote, ReceiptText, LucideIcon } from 'lucide-react'
import Capture from './pages/Capture'
import Outbox from './pages/Outbox'
import Commissions from './pages/Commissions'
import Receipts from './pages/Receipts'
import SyncPill from './components/SyncPill'
import LangSwitcher from './components/LangSwitcher'
import { queuedItems } from './db'

const tabs: { to: string; key: string; icon: LucideIcon }[] = [
  { to: '/', key: 'capture', icon: Plus },
  { to: '/outbox', key: 'outbox', icon: ArrowDownUp },
  { to: '/commissions', key: 'commissions', icon: Banknote },
  { to: '/receipts', key: 'receipts', icon: ReceiptText },
]

export default function App() {
  const { t } = useTranslation('common')
  const [queued, setQueued] = useState(0)

  useEffect(() => {
    let alive = true
    const poll = () =>
      queuedItems('queued')
        .then((items) => alive && setQueued(items.length))
        .catch(() => {})
    poll()
    const id = setInterval(poll, 5000)
    return () => {
      alive = false
      clearInterval(id)
    }
  }, [])

  return (
    <div className="min-h-screen pb-20 max-w-lg mx-auto">
      <header className="bg-brand-800 text-neutral-50 px-4 py-4 sticky top-0 z-10 shadow-sm">
        <div className="flex items-start justify-between gap-2">
          <div>
            <h1 className="text-lg font-bold">{t('app.title')}</h1>
            <p className="text-xs text-brand-100">{t('app.subtitle')}</p>
          </div>
          <div className="flex flex-col items-end gap-1.5">
            <SyncPill queued={queued} />
            <LangSwitcher />
          </div>
        </div>
      </header>
      <main className="p-4">
        <Routes>
          <Route path="/" element={<Capture />} />
          <Route path="/outbox" element={<Outbox />} />
          <Route path="/commissions" element={<Commissions />} />
          <Route path="/receipts" element={<Receipts />} />
        </Routes>
      </main>
      <nav className="fixed bottom-0 left-0 right-0 bg-white border-t border-neutral-200 max-w-lg mx-auto">
        <div className="grid grid-cols-4">
          {tabs.map(({ to, key, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              end={to === '/'}
              className={({ isActive }) =>
                `flex flex-col items-center py-2.5 text-xs focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand-700 focus-visible:outline-none ${
                  isActive ? 'text-brand-700 font-bold' : 'text-stone-600'
                }`
              }
            >
              <Icon aria-hidden="true" className="h-5 w-5 mb-0.5" />
              {t(`nav.${key}`)}
            </NavLink>
          ))}
        </div>
      </nav>
    </div>
  )
}
