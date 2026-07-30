// Keycloak OIDC login (authorization code + PKCE) via oidc-client-ts,
// active only when VITE_AUTH_MODE=keycloak (HARDENING H2). The dev-token
// path (X-Dev-Role header) stays the default when unset. Tokens are kept
// in memory (never localStorage); silent renew is enabled in prod mode.
import { InMemoryWebStorage, User, UserManager, WebStorageStateStore } from 'oidc-client-ts'

export const AUTH_MODE = (import.meta.env.VITE_AUTH_MODE as string) || 'dev'

let manager: UserManager | null = null
let currentUser: User | null = null

// initAuth boots the OIDC flow in keycloak mode. It resolves once the app
// may render; when a redirect to Keycloak is required it never resolves
// (the browser is navigating away).
export async function initAuth(): Promise<void> {
  if (AUTH_MODE !== 'keycloak') return
  manager = new UserManager({
    authority: (import.meta.env.VITE_KEYCLOAK_ISSUER as string) || '',
    client_id: (import.meta.env.VITE_KEYCLOAK_CLIENT_ID as string) || 'agent-pwa',
    redirect_uri: window.location.origin + window.location.pathname,
    post_logout_redirect_uri: window.location.origin + window.location.pathname,
    response_type: 'code', // authorization code + PKCE (S256 by default)
    scope: 'openid profile email',
    automaticSilentRenew: true,
    userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
  })
  manager.events.addUserLoaded((u) => {
    currentUser = u
  })
  // Authorization-code redirect callback
  if (window.location.search.includes('code=') && window.location.search.includes('state=')) {
    currentUser = await manager.signinRedirectCallback(window.location.href)
    window.history.replaceState({}, document.title, window.location.pathname + window.location.hash)
    return
  }
  currentUser = await manager.getUser()
  if (!currentUser || currentUser.expired) {
    await manager.signinRedirect()
    return new Promise<void>(() => {}) // navigating to Keycloak
  }
}

// getAccessToken returns the current Keycloak access token, or null in
// dev mode (callers then fall back to the X-Dev-Role dev token).
export function getAccessToken(): string | null {
  if (AUTH_MODE !== 'keycloak') return null
  return currentUser && !currentUser.expired ? currentUser.access_token : null
}

export async function logout(): Promise<void> {
  if (manager) await manager.signoutRedirect()
}
