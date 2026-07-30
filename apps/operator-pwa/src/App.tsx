import { NavLink, Route, Routes } from 'react-router-dom'
import Profile from './pages/Profile'
import Band from './pages/Band'
import Pay from './pages/Pay'
import Education from './pages/Education'

const tabs = [
  { to: '/', label: 'Profile', icon: '◉' },
  { to: '/band', label: 'My Band', icon: '▦' },
  { to: '/pay', label: 'Pay & Certificate', icon: '₦' },
  { to: '/education', label: 'Learn', icon: '?' },
]

export default function App() {
  return (
    <div className="min-h-screen pb-20 max-w-lg mx-auto">
      <header className="bg-sand-700 text-sand-50 px-4 py-4 sticky top-0 z-10">
        <h1 className="text-lg font-bold">Meridian Operator</h1>
        <p className="text-xs text-sand-200">Self-service — presumptive levy & certificates</p>
      </header>
      <main className="p-4">
        <Routes>
          <Route path="/" element={<Profile />} />
          <Route path="/band" element={<Band />} />
          <Route path="/pay" element={<Pay />} />
          <Route path="/education" element={<Education />} />
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
