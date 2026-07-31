/** @type {import('tailwindcss').Config} */
// Meridian One §2 — token block (hex values are the single source of truth,
// identical in every Meridian app). `sand` renamed to `neutral`; `brand` is
// the deep-green identity scale; semantic colors are status-only.
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        brand: {
          50: '#f0fdf4', 100: '#dcfce7', 200: '#bbf7d0', 300: '#86efac',
          400: '#4ade80', 500: '#22c55e', 600: '#16a34a', 700: '#15803d',
          800: '#166534', 900: '#14532d', 950: '#052e16',
        },
        neutral: {
          50: '#faf7f2', 100: '#f4eee3', 200: '#e9dcc8', 300: '#d9c9ae',
          400: '#c3ae8b', 500: '#a98f66', 600: '#8a6d3b', 700: '#6f5830',
          800: '#554325', 900: '#3b2f1b',
        },
        // Semantic (text-on-white values pre-verified ≥4.5:1; surfaces pair
        // with their on-surface text token).
        success: { DEFAULT: '#f0fdf4', strong: '#15803d', on: '#166534' },
        warning: { DEFAULT: '#fffbeb', strong: '#92400e', on: '#92400e' },
        danger: { DEFAULT: '#fef2f2', strong: '#b91c1c', on: '#991b1b' },
        info: { DEFAULT: '#eff6ff', strong: '#1d4ed8', on: '#1e40af' },
      },
      fontFamily: { sans: ['ui-sans-serif', 'system-ui', 'sans-serif'] },
    },
  },
  plugins: [],
}
