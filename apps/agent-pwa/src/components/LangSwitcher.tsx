import { useTranslation } from 'react-i18next'
import { LANGS, setLang, Lang } from '../i18n'

/** Language switcher (spec §10) — persisted per-device; all languages LTR. */
export default function LangSwitcher() {
  const { t, i18n } = useTranslation('common')
  return (
    <label className="inline-flex items-center gap-1 text-xs text-brand-100">
      <span>{t('lang.label')}</span>
      <select
        className="bg-brand-900 text-white text-xs rounded-md px-1.5 py-1 border border-brand-700 focus-visible:ring-2 focus-visible:ring-brand-400 focus-visible:outline-none"
        value={i18n.language}
        onChange={(e) => setLang(e.target.value as Lang)}
        aria-label={t('lang.label')}
      >
        {LANGS.map((l) => (
          <option key={l} value={l}>
            {t(`lang.${l}`)}
          </option>
        ))}
      </select>
    </label>
  )
}
