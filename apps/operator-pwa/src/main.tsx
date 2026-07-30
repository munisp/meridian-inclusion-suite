import React from 'react'
import ReactDOM from 'react-dom/client'
import { HashRouter } from 'react-router-dom'
import App from './App'
import { initAuth } from './auth'
import './index.css'

// VITE_AUTH_MODE=keycloak runs the OIDC PKCE flow first (may redirect);
// dev mode resolves immediately and the X-Dev-Role token is used.
initAuth().then(() => {
  ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
      <HashRouter>
        <App />
      </HashRouter>
    </React.StrictMode>,
  )
})

if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('./sw.js').catch((err) => console.warn('sw register failed', err))
  })
}
