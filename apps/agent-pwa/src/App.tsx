import { NavLink, Route, Routes } from 'react-router-dom'
import Capture from './pages/Capture'
import Outbox from './pages/Outbox'
import Commissions from './pages/Commissions'
import Receipts from './pages/Receipts'

const tabs = [
  { to: '/', label: 'Capture', icon: '＋' },
  { to: '/outbox', label: 'Outbox', icon: '⇅' },
  { to: '/commissions', label: 'Commissions', icon: '₦' },
  { to: '/receipts', label: 'Receipts', icon: '▤' },
]

export default function App() {
  return (
    <div className="min-h-screen pb-20 max-w-lg mx-auto">
      <header className="bg-sand-700 text-sand-50 px-4 py-4 sticky top-0 z-10">
        <h1 className="text-lg font-bold">Meridian Field Agent</h1>
        <p className="text-xs text-sand-200">NRS onboarding — offline-first capture</p>
      </header>
      <main className="p-4">
        <Routes>
          <Route path="/" element={<Capture />} />
          <Route path="/outbox" element={<Outbox />} />
          <Route path="/commissions" element={<Commissions />} />
          <Route path="/receipts" element={<Receipts />} />
        </Routes>
      </main>
      <nav className="fixed bottom-0 left-0 right-0 bg-white border-t border-sand-200 max-w-lg mx-auto">
        <div className="grid grid-cols-4">
          {tabs.map((t) => (
            <NavLink
              key={t.to}
              to={t.to}
              end={t.to === '/'}
              className={({ isActive }) =>
                `flex flex-col items-center py-2.5 text-xs ${
                  isActive ? 'text-sand-700 font-bold' : 'text-stone-500'
                }`
              }
            >
              <span className="text-lg leading-none">{t.icon}</span>
              {t.label}
            </NavLink>
          ))}
        </div>
      </nav>
    </div>
  )
}
