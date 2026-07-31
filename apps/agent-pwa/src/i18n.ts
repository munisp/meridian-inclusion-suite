// Meridian One §10 — i18next scaffold. EN default/fallback; HA/YO/IG ship
// as bundles (offline-safe, no lazy fetches). All four languages are LTR.
// Persisted per-device under `app.lang`.
import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import en from './locales/en/common.json'
import ha from './locales/ha/common.json'
import yo from './locales/yo/common.json'
import ig from './locales/ig/common.json'

export const LANGS = ['en', 'ha', 'yo', 'ig'] as const
export type Lang = (typeof LANGS)[number]

i18n.use(initReactI18next).init({
  resources: {
    en: { common: en },
    ha: { common: ha },
    yo: { common: yo },
    ig: { common: ig },
  },
  lng: (localStorage.getItem('app.lang') as Lang) || 'en',
  fallbackLng: 'en',
  ns: ['common'],
  defaultNS: 'common',
  interpolation: { escapeValue: false },
})

export function setLang(l: Lang) {
  localStorage.setItem('app.lang', l)
  i18n.changeLanguage(l)
}

export default i18n
