/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        sand: {
          50: '#faf7f2', 100: '#f4eee3', 200: '#e9dcc8', 300: '#d9c9ae',
          400: '#c3ae8b', 500: '#a98f66', 600: '#8a6d3b', 700: '#6f5830',
          800: '#554325', 900: '#3b2f1b',
        },
      },
      fontFamily: { sans: ['ui-sans-serif', 'system-ui', 'sans-serif'] },
    },
  },
  plugins: [],
}
