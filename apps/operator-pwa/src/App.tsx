import { NavLink, Route, Routes } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { User, Layers, Banknote, BookOpen, LucideIcon } from 'lucide-react'
import Profile from './pages/Profile'
import Band from './pages/Band'
import Pay from './pages/Pay'
import Education from './pages/Education'
import SyncPill from './components/SyncPill'
import LangSwitcher from './components/LangSwitcher'

const tabs: { to: string; key: string; icon: LucideIcon }[] = [
  { to: '/', key: 'profile', icon: User },
  { to: '/band', key: 'band', icon: Layers },
  { to: '/pay', key: 'pay', icon: Banknote },
  { to: '/education', key: 'education', icon: BookOpen },
]

export default function App() {
  const { t } = useTranslation('common')
  return (
    <div className="min-h-screen pb-20 max-w-lg mx-auto">
      <header className="bg-brand-800 text-neutral-50 px-4 py-4 sticky top-0 z-10 shadow-sm">
        <div className="flex items-start justify-between gap-2">
          <div>
            <h1 className="text-lg font-bold">{t('app.title')}</h1>
            <p className="text-xs text-brand-100">{t('app.subtitle')}</p>
          </div>
          <div className="flex flex-col items-end gap-1.5">
            <SyncPill />
            <LangSwitcher />
          </div>
        </div>
      </header>
      <main className="p-4">
        <Routes>
          <Route path="/" element={<Profile />} />
          <Route path="/band" element={<Band />} />
          <Route path="/pay" element={<Pay />} />
          <Route path="/education" element={<Education />} />
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
